//go:build linux

package buildah

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

const maxSimpleCopyXattrSize = 64 * 1024 * 1024

// simpleLocalCopyDigestCandidates computes the only input tar streams Buildah
// can produce for a deliberately narrow local COPY: one plain regular file,
// with no option or context-policy transformations.  The destination state
// decides whether Buildah retains the source basename or renames it to the
// destination basename, so both are returned when they differ.
//
// eligible is false whenever the source cannot be snapshotted with enough
// confidence to reproduce Buildah's copier stream exactly.  Callers must then
// use Buildah's authoritative digest path.
func simpleLocalCopyDigestCandidates(contextDir string, artifacts []string, operation Copy) (candidates []digest.Digest, eligible bool) {
	if contextDir == "" || len(operation.InlineFiles) != 0 || len(operation.Sources) != 1 || operation.Destination == "" ||
		operation.Chown != "" || operation.Chmod != "" || operation.Link || operation.Parents || len(operation.Excludes) != 0 {
		return nil, false
	}
	source := operation.Sources[0]
	if source == "" || filepath.IsAbs(source) || filepath.Clean(source) != source || source == "." ||
		strings.ContainsAny(source, "*?[") || source == ".." || strings.HasPrefix(source, ".."+string(filepath.Separator)) {
		return nil, false
	}
	policy, err := prepareContextPolicyWithIgnore(contextDir, artifacts, operation.IgnoreFile)
	if err != nil || len(policy.authoredExcludes) != 0 || len(policy.protectedExcludes) != 0 {
		return nil, false
	}

	root, err := filepath.Abs(contextDir)
	if err != nil {
		return nil, false
	}
	file, ok := openConfinedSimpleCopySource(root, source)
	if !ok {
		return nil, false
	}
	defer func() { _ = file.Close() }()

	beforeFile, err := file.Stat()
	if err != nil || !beforeFile.Mode().IsRegular() {
		return nil, false
	}
	xattrs, err := fgetSimpleCopyXattrs(int(file.Fd()))
	if err != nil {
		return nil, false
	}

	names := []string{filepath.Base(source)}
	if !strings.HasSuffix(operation.Destination, string(filepath.Separator)) {
		destinationName := filepath.Base(filepath.Clean(operation.Destination))
		if destinationName != "." && destinationName != string(filepath.Separator) && destinationName != names[0] {
			names = append(names, destinationName)
		}
	}
	digesters := make([]digest.Digester, len(names))
	writers := make([]*tar.Writer, len(names))
	contentWriters := make([]io.Writer, len(names))
	for index, name := range names {
		header, ok := simpleLocalCopyHeader(beforeFile, xattrs, name)
		if !ok {
			return nil, false
		}
		digesters[index] = digest.SHA256.Digester()
		writers[index] = tar.NewWriter(digesters[index].Hash())
		if err := writers[index].WriteHeader(header); err != nil {
			return nil, false
		}
		contentWriters[index] = writers[index]
	}

	written, err := io.Copy(io.MultiWriter(contentWriters...), file)
	if err != nil || written != beforeFile.Size() {
		return nil, false
	}
	for _, writer := range writers {
		if err := writer.Close(); err != nil {
			return nil, false
		}
	}

	afterFile, err := file.Stat()
	if err != nil || !stableCopySource(beforeFile, afterFile) {
		return nil, false
	}
	afterXattrs, err := fgetSimpleCopyXattrs(int(file.Fd()))
	if err != nil || !reflect.DeepEqual(xattrs, afterXattrs) {
		return nil, false
	}
	pathFile, ok := openConfinedSimpleCopySource(root, source)
	if !ok {
		return nil, false
	}
	defer func() { _ = pathFile.Close() }()
	pathInfo, err := pathFile.Stat()
	if err != nil || !stableCopySource(afterFile, pathInfo) {
		return nil, false
	}
	pathXattrs, err := fgetSimpleCopyXattrs(int(pathFile.Fd()))
	if err != nil || !reflect.DeepEqual(afterXattrs, pathXattrs) {
		return nil, false
	}
	for index := range names {
		candidates = append(candidates, digesters[index].Digest())
	}
	return candidates, true
}

func simpleLocalCopyHeader(info os.FileInfo, xattrs map[string]string, name string) (*tar.Header, bool) {
	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return nil, false
	}
	header.Name = filepath.ToSlash(name)
	header.Mode &^= 0o6000
	header.Uname, header.Gname = "", ""
	header.Uid, header.Gid = 0, 0
	if len(xattrs) != 0 {
		header.PAXRecords = make(map[string]string, len(xattrs))
		for key, value := range xattrs {
			header.PAXRecords["SCHILY.xattr."+key] = value
		}
	}
	// storage/pkg/archive.ReadFileFlagsToTarHeader is a no-op on Linux.  Do
	// not call its path-based API here: the confined fd is the source of truth.
	return header, true
}

func openConfinedSimpleCopySource(root, relative string) (*os.File, bool) {
	baseFD, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	defer func() { _ = unix.Close(baseFD) }()
	rootRelative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(root)), "/")
	if rootRelative == "" {
		rootRelative = "."
	}
	rootFD, err := unix.Openat2(baseFD, rootRelative, &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, false
	}
	defer func() { _ = unix.Close(rootFD) }()
	pathFD, err := unix.Openat2(rootFD, filepath.ToSlash(relative), &unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, false
	}
	pathFile := os.NewFile(uintptr(pathFD), filepath.Join(root, relative))
	if pathFile == nil {
		_ = unix.Close(pathFD)
		return nil, false
	}
	pathInfo, err := pathFile.Stat()
	if err != nil || !pathInfo.Mode().IsRegular() {
		_ = pathFile.Close()
		return nil, false
	}
	// Reopen the already-confined O_PATH descriptor instead of resolving the
	// authored path a second time.  O_NONBLOCK is retained as defense in depth;
	// the pinned inode was already verified to be a regular file.
	fd, err := unix.Open("/proc/self/fd/"+strconv.Itoa(pathFD), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	_ = pathFile.Close()
	if err != nil {
		return nil, false
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root, relative))
	if file == nil {
		_ = unix.Close(fd)
		return nil, false
	}
	readInfo, err := file.Stat()
	if err != nil || !stableCopySource(pathInfo, readInfo) {
		_ = file.Close()
		return nil, false
	}
	return file, true
}

func fgetSimpleCopyXattrs(fd int) (map[string]string, error) {
	listSize := 64 * 1024
	var list []byte
	for listSize < maxSimpleCopyXattrSize {
		list = make([]byte, listSize)
		size, err := unix.Flistxattr(fd, list)
		if err != nil {
			if errors.Is(err, syscall.ERANGE) {
				listSize *= 2
				continue
			}
			if errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.ENOSYS) {
				return map[string]string{}, nil
			}
			return nil, err
		}
		list = list[:size]
		break
	}
	if listSize >= maxSimpleCopyXattrSize {
		return nil, syscall.E2BIG
	}
	result := make(map[string]string)
	for attribute := range strings.SplitSeq(string(list), "\x00") {
		if !simpleCopyRelevantXattr(attribute) {
			continue
		}
		valueSize := 64 * 1024
		for valueSize < maxSimpleCopyXattrSize {
			value := make([]byte, valueSize)
			size, err := unix.Fgetxattr(fd, attribute, value)
			if err != nil {
				if errors.Is(err, syscall.ERANGE) {
					valueSize *= 2
					continue
				}
				return nil, err
			}
			result[attribute] = string(value[:size])
			break
		}
		if valueSize >= maxSimpleCopyXattrSize {
			return nil, syscall.E2BIG
		}
	}
	return result, nil
}

func simpleCopyRelevantXattr(attribute string) bool {
	irrelevant, err := filepath.Match("user.overlay.*", attribute)
	if err != nil || irrelevant {
		return false
	}
	user, err := filepath.Match("user.*", attribute)
	return err == nil && (attribute == "security.capability" || attribute == "security.ima" || user)
}

func stableCopySource(before, after os.FileInfo) bool {
	if before == nil || after == nil || !os.SameFile(before, after) || before.Mode() != after.Mode() ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return false
	}
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	if !beforeOK || !afterOK {
		return false
	}
	return beforeStat.Dev == afterStat.Dev && beforeStat.Ino == afterStat.Ino && beforeStat.Mode == afterStat.Mode &&
		beforeStat.Size == afterStat.Size && beforeStat.Mtim == afterStat.Mtim && beforeStat.Ctim == afterStat.Ctim
}
