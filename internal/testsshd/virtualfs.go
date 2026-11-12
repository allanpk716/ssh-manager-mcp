package testsshd

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// virtualFilesystemHandler 把 SFTP 的绝对路径映射到一个临时目录，让测试能
// 使用生产代码固定的挂载路径，同时不写测试进程所在机器的其他目录。
type virtualFilesystemHandler struct {
	root string
}

func newVirtualFilesystemHandler(root string) sftp.Handlers {
	handler := &virtualFilesystemHandler{root: root}
	return sftp.Handlers{FileGet: handler, FilePut: handler, FileCmd: handler, FileList: handler}
}

func (handler *virtualFilesystemHandler) hostPath(remotePath string) string {
	cleanRemote := path.Clean(strings.TrimPrefix(path.Clean("/"+remotePath), "/"))
	if cleanRemote == "" || cleanRemote == "." {
		return handler.root
	}
	return filepath.Join(handler.root, filepath.FromSlash(cleanRemote))
}

func (handler *virtualFilesystemHandler) openFlags(request *sftp.Request) int {
	flags := request.Pflags()
	openFlags := os.O_RDONLY
	writes := flags.Write || flags.Append
	switch {
	case flags.Read && writes:
		openFlags = os.O_RDWR
	case writes:
		openFlags = os.O_WRONLY
	}
	if flags.Append {
		openFlags |= os.O_APPEND
	}
	if flags.Creat {
		openFlags |= os.O_CREATE
	}
	if flags.Trunc {
		openFlags |= os.O_TRUNC
	}
	if flags.Excl {
		openFlags |= os.O_EXCL
	}
	return openFlags
}

func (handler *virtualFilesystemHandler) Fileread(request *sftp.Request) (io.ReaderAt, error) {
	file, err := os.Open(handler.hostPath(request.Filepath))
	if err != nil {
		return nil, err
	}
	return &delayedReaderAt{file: file, delay: 500 * time.Microsecond}, nil
}

func (handler *virtualFilesystemHandler) Filewrite(request *sftp.Request) (io.WriterAt, error) {
	return os.OpenFile(handler.hostPath(request.Filepath), handler.openFlags(request), 0o644)
}

func (handler *virtualFilesystemHandler) OpenFile(request *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return os.OpenFile(handler.hostPath(request.Filepath), handler.openFlags(request), 0o644)
}

func (handler *virtualFilesystemHandler) Filecmd(request *sftp.Request) error {
	sourcePath := handler.hostPath(request.Filepath)
	targetPath := ""
	if request.Target != "" {
		targetPath = handler.hostPath(request.Target)
	}
	switch request.Method {
	case "Setstat":
		return nil
	case "Rename", "PosixRename":
		return os.Rename(sourcePath, targetPath)
	case "Rmdir":
		return os.Remove(sourcePath)
	case "Mkdir":
		return os.Mkdir(sourcePath, 0o755)
	case "Link":
		return os.Link(sourcePath, targetPath)
	case "Symlink":
		return os.Symlink(sourcePath, targetPath)
	case "Remove":
		return os.Remove(sourcePath)
	default:
		return os.ErrInvalid
	}
}

func (handler *virtualFilesystemHandler) StatVFS(request *sftp.Request) (*sftp.StatVFS, error) {
	return nil, os.ErrInvalid
}

func (handler *virtualFilesystemHandler) Filelist(request *sftp.Request) (sftp.ListerAt, error) {
	if request.Method == "List" {
		entries, err := os.ReadDir(handler.hostPath(request.Filepath))
		if err != nil {
			return nil, err
		}
		files := make([]os.FileInfo, 0, len(entries))
		for _, entry := range entries {
			file, infoErr := entry.Info()
			if infoErr != nil {
				return nil, infoErr
			}
			files = append(files, file)
		}
		return &virtualFilesystemLister{files: files}, nil
	}
	file, err := os.Stat(handler.hostPath(request.Filepath))
	if err != nil {
		return nil, err
	}
	return &virtualFilesystemLister{files: []os.FileInfo{file}}, nil
}

type virtualFilesystemLister struct {
	files []os.FileInfo
}

type delayedReaderAt struct {
	file  *os.File
	delay time.Duration
}

// ReadAt 在真实读取前短暂等待，让停止后续传的端到端测试能可靠落在两个
// 块边界之间；等待只存在于测试用虚拟文件系统，不影响生产代码。
func (reader *delayedReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	time.Sleep(reader.delay)
	return reader.file.ReadAt(buffer, offset)
}

func (reader *delayedReaderAt) Close() error {
	return reader.file.Close()
}

func (lister *virtualFilesystemLister) ListAt(target []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(lister.files)) {
		return 0, io.EOF
	}
	count := copy(target, lister.files[offset:])
	if offset+int64(count) == int64(len(lister.files)) {
		return count, io.EOF
	}
	return count, nil
}
