// +build cgo
// +build linux darwin freebsd

package cmount

import (
	"os"
	"path"
	"sync"
	"time"

	"github.com/billziss-gh/cgofuse/fuse"
	"github.com/ncw/rclone/cmd/mountlib"
	"github.com/ncw/rclone/fs"
	"github.com/pkg/errors"
)

const fhUnset = ^uint64(0)

// FS represents the top level filing system
type FS struct {
	fuse.FileSystemBase
	FS          *mountlib.FS
	f           fs.Fs
	openDirs    *OpenFiles
	openFilesWr *OpenFiles
	openFilesRd *OpenFiles
	ready       chan (struct{})
}

// NewFS makes a new FS
func NewFS(f fs.Fs) *FS {
	fsys := &FS{
		FS:          mountlib.NewFS(f),
		f:           f,
		openDirs:    NewOpenFiles(0x01),
		openFilesWr: NewOpenFiles(0x02),
		openFilesRd: NewOpenFiles(0x03),
		ready:       make(chan (struct{})),
	}
	if noSeek {
		fsys.FS.NoSeek()
	}
	return fsys
}

type OpenFiles struct {
	mu    sync.Mutex
	mark  uint8
	nodes []interface{}
}

func NewOpenFiles(mark uint8) *OpenFiles {
	return &OpenFiles{
		mark: mark,
	}
}

// Open a node returning a file handle
func (of *OpenFiles) Open(node interface{}) (fh uint64) {
	of.mu.Lock()
	defer of.mu.Unlock()
	var i int
	var oldNode interface{}
	for i, oldNode = range of.nodes {
		if oldNode == nil {
			of.nodes[i] = node
			goto found
		}
	}
	of.nodes = append(of.nodes, node)
	i = len(of.nodes) - 1
found:
	return uint64((i << 8) | int(of.mark))
}

// get the node for fh, call with the lock held
func (of *OpenFiles) get(fh uint64) (i int, node interface{}, errc int) {
	receivedMark := uint8(fh)
	if receivedMark != of.mark {
		return i, nil, -fuse.EBADF
	}
	i64 := fh >> 8
	if i64 > uint64(len(of.nodes)) {
		return i, nil, -fuse.EBADF
	}
	i = int(i64)
	node = of.nodes[i]
	if node == nil {
		return i, nil, -fuse.EBADF
	}
	return i, node, 0
}

// Get the node for the file handle
func (of *OpenFiles) Get(fh uint64) (node interface{}, errc int) {
	of.mu.Lock()
	_, node, errc = of.get(fh)
	of.mu.Unlock()
	return
}

// Close the node
func (of *OpenFiles) Close(fh uint64) (errc int) {
	of.mu.Lock()
	i, _, errc := of.get(fh)
	if errc == 0 {
		of.nodes[i] = nil
	}
	of.mu.Unlock()
	return
}

// lookup a Node given a path
func (fsys *FS) lookupNode(path string) (node mountlib.Node, errc int) {
	node, err := fsys.FS.Lookup(path)
	return node, translateError(err)
}

// lookup a Dir given a path
func (fsys *FS) lookupDir(path string) (dir *mountlib.Dir, errc int) {
	node, errc := fsys.lookupNode(path)
	if errc != 0 {
		return nil, errc
	}
	dir, ok := node.(*mountlib.Dir)
	if !ok {
		return nil, -fuse.ENOTDIR
	}
	return dir, 0
}

// lookup a parent Dir given a path returning the dir and the leaf
func (fsys *FS) lookupParentDir(filePath string) (leaf string, dir *mountlib.Dir, errc int) {
	parentDir, leaf := path.Split(filePath)
	dir, errc = fsys.lookupDir(parentDir)
	return leaf, dir, errc
}

// lookup a File given a path
func (fsys *FS) lookupFile(path string) (file *mountlib.File, errc int) {
	node, errc := fsys.lookupNode(path)
	if errc != 0 {
		return nil, errc
	}
	file, ok := node.(*mountlib.File)
	if !ok {
		return nil, -fuse.EISDIR
	}
	return file, 0
}

// get a read or write file handle
func (fsys *FS) getReadOrWriteFh(fh uint64) (handle interface{}, errc int) {
	handle, errc = fsys.openFilesRd.Get(fh)
	if errc == 0 {
		return
	}
	return fsys.openFilesWr.Get(fh)
}

// get a node from the path or from the fh if not fhUnset
func (fsys *FS) getNode(path string, fh uint64) (node mountlib.Node, errc int) {
	if fh == fhUnset {
		node, errc = fsys.lookupNode(path)
	} else {
		var n interface{}
		n, errc = fsys.getReadOrWriteFh(fh)
		if errc == 0 {
			if get, ok := n.(mountlib.Noder); ok {
				node = get.Node()
			} else {
				fs.Errorf(path, "Bad node type %T", n)
				errc = -fuse.EIO
			}
		}
	}
	return
}

// stat fills up the stat block for Node
func (fsys *FS) stat(node mountlib.Node, stat *fuse.Stat_t) (errc int) {
	var Size uint64
	var Blocks uint64
	var modTime time.Time
	var Mode os.FileMode
	switch x := node.(type) {
	case *mountlib.Dir:
		modTime = x.ModTime()
		Mode = dirPerms | fuse.S_IFDIR
	case *mountlib.File:
		var err error
		modTime, Size, Blocks, err = x.Attr(noModTime)
		if err != nil {
			return translateError(err)
		}
		Mode = filePerms | fuse.S_IFREG
	}
	//stat.Dev = 1
	stat.Ino = node.Inode() // FIXME do we need to set the inode number?
	stat.Mode = uint32(Mode)
	stat.Nlink = 1
	stat.Uid = uid
	stat.Gid = gid
	//stat.Rdev
	stat.Size = int64(Size)
	t := fuse.NewTimespec(modTime)
	stat.Atim = t
	stat.Mtim = t
	stat.Ctim = t
	stat.Blksize = 512
	stat.Blocks = int64(Blocks)
	stat.Birthtim = t
	// fs.Debugf(nil, "stat = %+v", *stat)
	return 0
}

// Init is called after the filesystem is ready
func (fsys *FS) Init() {
	close(fsys.ready)
}

// Destroy() call when it is unmounted (note that depending on how the
// file system is terminated the file system may not receive the
// Destroy() call).
func (fsys *FS) Destroy() {
}

// Getattr reads the attributes for path
func (fsys *FS) Getattr(path string, stat *fuse.Stat_t, fh uint64) (errc int) {
	fs.Debugf(path, "Getattr(path=%q,fh=%d)", path, fh)
	node, errc := fsys.getNode(path, fh)
	if errc == 0 {
		errc = fsys.stat(node, stat)
	}
	fs.Debugf(path, "Getattr returns %d", errc)
	return
}

// Opendir opens path as a directory
func (fsys *FS) Opendir(path string) (errc int, fh uint64) {
	fs.Debugf(path, "Opendir()")
	dir, errc := fsys.lookupDir(path)
	if errc == 0 {
		fh = fsys.openDirs.Open(dir)
	} else {
		fh = fhUnset
	}
	fs.Debugf(path, "Opendir returns errc=%d, fh=%d", errc, fh)
	return
}

// Readdir reads the directory at dirPath
func (fsys *FS) Readdir(dirPath string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) (errc int) {
	fs.Debugf(dirPath, "Readdir(ofst=%d,fh=%d)", ofst, fh)

	node, errc := fsys.openDirs.Get(fh)
	if errc != 0 {
		return errc
	}

	dir, ok := node.(*mountlib.Dir)
	if !ok {
		return -fuse.ENOTDIR
	}

	items, err := dir.ReadDirAll()
	if err != nil {
		return translateError(err)
	}

	// Optionally, create a struct stat that describes the file as
	// for getattr (but FUSE only looks at st_ino and the
	// file-type bits of st_mode).
	//
	// FIXME If you call host.SetCapReaddirPlus() then WinFsp will
	// use the full stat information - a Useful optimization on
	// Windows.
	//
	// NB we are using the first mode for readdir: The readdir
	// implementation ignores the offset parameter, and passes
	// zero to the filler function's offset. The filler function
	// will not return '1' (unless an error happens), so the whole
	// directory is read in a single readdir operation.
	fill(".", nil, 0)
	fill("..", nil, 0)
	for _, item := range items {
		name := path.Base(item.Obj.Remote())
		fill(name, nil, 0)
	}
	return 0
}

// Releasedir finished reading the directory
func (fsys *FS) Releasedir(path string, fh uint64) (errc int) {
	fs.Debugf(path, "Releasedir(fh=%d)", fh)
	return fsys.openDirs.Close(fh)
}

// Statfs reads overall stats on the filessystem
// FIXME doesn't seem to be ever called
func (fsys *FS) Statfs(path string, stat *fuse.Statfs_t) (errc int) {
	fs.Debugf(path, "Statfs()")
	const blockSize = 4096
	const fsBlocks = (1 << 50) / blockSize
	stat.Blocks = fsBlocks  // Total data blocks in file system.
	stat.Bfree = fsBlocks   // Free blocks in file system.
	stat.Bavail = fsBlocks  // Free blocks in file system if you're not root.
	stat.Files = 1E9        // Total files in file system.
	stat.Ffree = 1E9        // Free files in file system.
	stat.Bsize = blockSize  // Block size
	stat.Namemax = 255      // Maximum file name length?
	stat.Frsize = blockSize // Fragment size, smallest addressable data size in the file system.
	return 0
}

func (fsys *FS) Open(path string, flags int) (errc int, fh uint64) {
	file, errc := fsys.lookupFile(path)
	if errc != 0 {
		return errc, fhUnset
	}
	rdwrMode := flags & fuse.O_ACCMODE
	var err error
	var handle interface{}
	switch {
	case rdwrMode == fuse.O_RDONLY:
		handle, err = file.OpenRead()
		if err != nil {
			return translateError(err), fhUnset
		}
		return 0, fsys.openFilesRd.Open(handle)
	case rdwrMode == fuse.O_WRONLY || (rdwrMode == fuse.O_RDWR && (flags&fuse.O_TRUNC) != 0):
		handle, err = file.OpenWrite()
		if err != nil {
			return translateError(err), fhUnset
		}
		return 0, fsys.openFilesWr.Open(handle)
	case rdwrMode == fuse.O_RDWR:
		fs.Errorf(path, "Can't open for Read and Write")
		return -fuse.EPERM, fhUnset
	}
	fs.Errorf(path, "Can't figure out how to open with flags: 0x%X", flags)
	return -fuse.EPERM, fhUnset
}

// Create creates and opens a file.
func (fsys *FS) Create(filePath string, flags int, mode uint32) (errc int, fh uint64) {
	leaf, parentDir, errc := fsys.lookupParentDir(filePath)
	if errc != 0 {
		return errc, fhUnset
	}
	_, handle, err := parentDir.Create(leaf)
	if err != nil {
		return translateError(err), fhUnset
	}
	return 0, fsys.openFilesWr.Open(handle)
}

func (fsys *FS) Truncate(path string, size int64, fh uint64) (errc int) {
	node, errc := fsys.getNode(path, fh)
	if errc != 0 {
		return errc
	}
	file, ok := node.(*mountlib.File)
	if !ok {
		return -fuse.EIO
	}
	// Read the size so far
	_, currentSize, _, err := file.Attr(true)
	if err != nil {
		return translateError(err)
	}
	fs.Debugf(path, "truncate to %d, currentSize %d", size, currentSize)
	if int64(currentSize) != size {
		fs.Errorf(path, "Can't truncate files")
		return -fuse.EPERM
	}
	return 0
}

func (fsys *FS) Read(path string, buff []byte, ofst int64, fh uint64) (n int) {
	// FIXME detect seek
	handle, errc := fsys.openFilesRd.Get(fh)
	if errc != 0 {
		return errc
	}
	rfh, ok := handle.(*mountlib.ReadFileHandle)
	if !ok {
		// Can only read from read file handle
		return -fuse.EIO
	}
	data, err := rfh.Read(int64(len(buff)), ofst)
	if err != nil {
		return translateError(err)
	}
	n = copy(buff, data)
	return n
}

func (fsys *FS) Write(path string, buff []byte, ofst int64, fh uint64) (n int) {
	// FIXME detect seek
	handle, errc := fsys.openFilesWr.Get(fh)
	if errc != 0 {
		return errc
	}
	wfh, ok := handle.(*mountlib.WriteFileHandle)
	if !ok {
		// Can only write to write file handle
		return -fuse.EIO
	}
	// FIXME made Write return int and Read take int since must fit in RAM
	n64, err := wfh.Write(buff, ofst)
	if err != nil {
		return translateError(err)
	}
	return int(n64)
}

func (fsys *FS) Flush(path string, fh uint64) (errc int) {
	handle, errc := fsys.getReadOrWriteFh(fh)
	if errc != 0 {
		return errc
	}
	var err error
	switch x := handle.(type) {
	case *mountlib.ReadFileHandle:
		err = x.Flush()
	case *mountlib.WriteFileHandle:
		err = x.Flush()
	default:
		return -fuse.EIO
	}
	return translateError(err)
}

func (fsys *FS) Release(path string, fh uint64) (errc int) {
	handle, errc := fsys.getReadOrWriteFh(fh)
	if errc != 0 {
		return errc
	}
	var err error
	switch x := handle.(type) {
	case *mountlib.ReadFileHandle:
		err = x.Release()
	case *mountlib.WriteFileHandle:
		err = x.Release()
	default:
		return -fuse.EIO
	}
	return translateError(err)
}

// Unlink removes a file.
func (fsys *FS) Unlink(filePath string) int {
	fs.Debugf(filePath, "Unlink()")
	leaf, parentDir, errc := fsys.lookupParentDir(filePath)
	if errc != 0 {
		return errc
	}
	return translateError(parentDir.Remove(leaf))
}

// Mkdir creates a directory.
func (fsys *FS) Mkdir(dirPath string, mode uint32) (errc int) {
	fs.Debugf(dirPath, "Mkdir(0%o)", mode)
	leaf, parentDir, errc := fsys.lookupParentDir(dirPath)
	if errc != 0 {
		return errc
	}
	_, err := parentDir.Mkdir(leaf)
	return translateError(err)
}

// Rmdir removes a directory
func (fsys *FS) Rmdir(dirPath string) int {
	fs.Debugf(dirPath, "Rmdir()")
	leaf, parentDir, errc := fsys.lookupParentDir(dirPath)
	if errc != 0 {
		return errc
	}
	return translateError(parentDir.Remove(leaf))
}

// Rename renames a file.
func (fsys *FS) Rename(oldPath string, newPath string) (errc int) {
	oldLeaf, oldParentDir, errc := fsys.lookupParentDir(oldPath)
	if errc != 0 {
		return errc
	}
	newLeaf, newParentDir, errc := fsys.lookupParentDir(newPath)
	if errc != 0 {
		return errc
	}
	return translateError(oldParentDir.Rename(oldLeaf, newLeaf, newParentDir))
}

// Utimens changes the access and modification times of a file.
func (fsys *FS) Utimens(path string, tmsp []fuse.Timespec) (errc int) {
	fs.Debugf(path, "Utimens %+v", tmsp)
	node, errc := fsys.lookupNode(path)
	if errc != 0 {
		return errc
	}
	var t time.Time
	if tmsp == nil || len(tmsp) < 2 {
		t = time.Now()
	} else {
		t = tmsp[1].Time()
	}
	var err error
	switch x := node.(type) {
	case *mountlib.Dir:
		err = x.SetModTime(t)
	case *mountlib.File:
		err = x.SetModTime(t)
	}
	return translateError(err)
}

// Translate errors from mountlib
func translateError(err error) int {
	if err == nil {
		return 0
	}
	cause := errors.Cause(err)
	if mErr, ok := cause.(mountlib.Error); ok {
		switch mErr {
		case mountlib.OK:
			return 0
		case mountlib.ENOENT:
			return -fuse.ENOENT
		case mountlib.ENOTEMPTY:
			return -fuse.ENOTEMPTY
		case mountlib.EEXIST:
			return -fuse.EEXIST
		case mountlib.ESPIPE:
			return -fuse.ESPIPE
		case mountlib.EBADF:
			return -fuse.EBADF
		}
	}
	fs.Errorf(nil, "IO error: %v", err)
	return -fuse.EIO
}