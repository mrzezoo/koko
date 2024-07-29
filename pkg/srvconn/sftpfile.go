package srvconn

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"

	"github.com/jumpserver/koko/pkg/config"
	"github.com/jumpserver/koko/pkg/jms-sdk-go/model"
	"github.com/jumpserver/koko/pkg/jms-sdk-go/service"
)

const (
	SearchFolderName = "_Search"
)

var errNoAccountUser = errors.New("please select one of the account user")

type SearchResultDir struct {
	subDirs    map[string]os.FileInfo
	folderName string
	modeTime   time.Time
}

func (sd *SearchResultDir) Name() string {
	return sd.folderName
}

func (sd *SearchResultDir) Size() int64 { return 0 }

func (sd *SearchResultDir) Mode() os.FileMode {
	return os.FileMode(0444) | os.ModeDir
}

func (sd *SearchResultDir) ModTime() time.Time { return sd.modeTime }

func (sd *SearchResultDir) IsDir() bool { return true }

func (sd *SearchResultDir) Sys() interface{} {
	return &syscall.Stat_t{Uid: 0, Gid: 0}
}

func (sd *SearchResultDir) List() (res []os.FileInfo, err error) {
	for _, item := range sd.subDirs {
		res = append(res, item)
	}
	return
}

func (sd *SearchResultDir) SetSubDirs(subDirs map[string]os.FileInfo) {
	if sd.subDirs != nil {
		for _, dir := range sd.subDirs {
			if assetDir, ok := dir.(*AssetDir); ok {
				assetDir.close()
			}
		}
	}
	sd.subDirs = subDirs
}

func (sd *SearchResultDir) close() {
	for _, dir := range sd.subDirs {
		if assetDir, ok := dir.(*AssetDir); ok {
			assetDir.close()
		}
	}
}

func NewNodeDir(builders ...FolderBuilderOption) NodeDir {
	var dirConf folderOptions
	for i := range builders {
		builders[i](&dirConf)
	}
	return NodeDir{
		ID:          dirConf.ID,
		folderName:  dirConf.Name,
		subDirs:     map[string]os.FileInfo{},
		modeTime:    time.Now().UTC(),
		once:        new(sync.Once),
		loadSubFunc: dirConf.loadSubFunc,
	}
}

type FolderBuilderOption func(info *folderOptions)

type SubFoldersLoadFunc func() map[string]os.FileInfo

type folderOptions struct {
	ID          string
	Name        string
	RemoteAddr  string
	fromType    model.LabelField
	loadSubFunc SubFoldersLoadFunc

	asset *model.PermAsset

	token *model.ConnectToken

	accountUsername string
}

func WithFolderUsername(username string) FolderBuilderOption {
	return func(info *folderOptions) {
		info.accountUsername = username
	}
}

func WithFolderName(name string) FolderBuilderOption {
	return func(info *folderOptions) {
		info.Name = name
	}
}

func WithFolderID(id string) FolderBuilderOption {
	return func(info *folderOptions) {
		info.ID = id
	}
}

func WitRemoteAddr(addr string) FolderBuilderOption {
	return func(info *folderOptions) {
		info.RemoteAddr = addr
	}
}

func WithSubFoldersLoadFunc(loadFunc SubFoldersLoadFunc) FolderBuilderOption {
	return func(info *folderOptions) {
		info.loadSubFunc = loadFunc
	}
}

func WithAsset(asset model.PermAsset) FolderBuilderOption {
	return func(info *folderOptions) {
		info.asset = &asset
	}
}

func WithToken(token *model.ConnectToken) FolderBuilderOption {
	return func(info *folderOptions) {
		info.token = token
	}
}

func WithFromType(fromType model.LabelField) FolderBuilderOption {
	return func(info *folderOptions) {
		info.fromType = fromType
	}
}

func NewAssetDir(jmsService *service.JMService, user *model.User, opts ...FolderBuilderOption) AssetDir {
	var dirOpts folderOptions
	for _, setter := range opts {
		setter(&dirOpts)
	}
	conf := config.GetConf()
	detailAsset := dirOpts.asset
	var permAccounts []model.PermAccount
	if dirOpts.token != nil {
		account := dirOpts.token.Account
		actions := dirOpts.token.Actions
		permAccount := model.PermAccount{
			Name:       account.Name,
			Username:   account.Username,
			SecretType: account.SecretType.Value,
			Actions:    actions,
		}
		permAccounts = append(permAccounts, permAccount)
		detailAsset = dirOpts.asset
	}
	ctx, ctxCancel := context.WithCancel(context.Background())
	return AssetDir{
		opts:        dirOpts,
		user:        user,
		detailAsset: detailAsset,
		modeTime:    time.Now().UTC(),
		suMaps:      generateSubAccountsFolderMap(permAccounts),
		ShowHidden:  conf.ShowHiddenFile,

		sftpSessions: make(map[string]*SftpSession),
		jmsService:   jmsService,
		ctx:          ctx,
		ctxCancel:    ctxCancel,
	}
}

type SftpFile struct {
	*sftp.File
	FTPLog *model.FTPLog
}

type SftpConn struct {
	permAccount *model.PermAccount
	HomeDirPath string
	client      *sftp.Client
	sshClient   *SSHClient
	sshSession  *gossh.Session
	token       *model.ConnectToken
	isClosed    bool
	rootDirPath string

	nextExpiredTime time.Time
	status          string
}

func (s *SftpConn) IsExpired() bool {
	return time.Since(s.nextExpiredTime) > 0
}

func (s *SftpConn) UpdateExpiredTime() {
	s.nextExpiredTime = time.Now().Add(time.Duration(30) * time.Minute)
}

func (s *SftpConn) IsOverwriteFile() bool {
	resolution := s.token.ConnectOptions.FilenameConflictResolution
	return !strings.EqualFold(resolution, FilenamePolicySuffix)
}

// check if the path is root path and disable to remove

func (s *SftpConn) IsRootPath(path string) bool {
	return s.rootDirPath == path
}

const (
	FilenamePolicyReplace = "replace"
	FilenamePolicySuffix  = "suffix"
)

func (s *SftpConn) Close() {
	if s.client == nil {
		return
	}
	_ = s.client.Close()
	s.isClosed = true
}

func NewFakeFile(name string, isDir bool) *FakeFileInfo {
	return &FakeFileInfo{
		name:    name,
		modTime: time.Now().UTC(),
		isDir:   isDir,
		size:    int64(0),
	}
}

func NewFakeSymFile(name string) *FakeFileInfo {
	return &FakeFileInfo{
		name:    name,
		modTime: time.Now().UTC(),
		size:    int64(0),
		symlink: name,
	}
}

type FakeFileInfo struct {
	name    string
	isDir   bool
	size    int64
	modTime time.Time
	symlink string
}

func (f *FakeFileInfo) Name() string { return f.name }
func (f *FakeFileInfo) Size() int64  { return f.size }
func (f *FakeFileInfo) Mode() os.FileMode {
	ret := os.FileMode(0644)
	if f.isDir {
		ret = os.FileMode(0755) | os.ModeDir
	}
	if f.symlink != "" {
		ret = os.FileMode(0777) | os.ModeSymlink
	}
	return ret
}
func (f *FakeFileInfo) ModTime() time.Time { return f.modTime }
func (f *FakeFileInfo) IsDir() bool        { return f.isDir }
func (f *FakeFileInfo) Sys() interface{} {
	return &syscall.Stat_t{Uid: 0, Gid: 0}
}

type FileInfoList []os.FileInfo

func (fl FileInfoList) Len() int {
	return len(fl)
}
func (fl FileInfoList) Swap(i, j int)      { fl[i], fl[j] = fl[j], fl[i] }
func (fl FileInfoList) Less(i, j int) bool { return fl[i].Name() < fl[j].Name() }
