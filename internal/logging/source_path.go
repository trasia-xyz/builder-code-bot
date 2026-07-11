package logging

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

var (
	sourceRootMu         sync.Mutex
	sourceRootDir        string
	sourceModuleName     string
	sourceModuleNameOnce sync.Once
)

func trimSourceAttr(attr slog.Attr) slog.Attr {
	switch source := attr.Value.Any().(type) {
	case *slog.Source:
		if source == nil {
			return attr
		}
		next := *source
		next.File = trimSourceFile(next.File)
		return slog.Any(attr.Key, &next)
	case slog.Source:
		source.File = trimSourceFile(source.File)
		return slog.Any(attr.Key, source)
	default:
		return attr
	}
}

func source(pc uintptr) string {
	frames := runtime.CallersFrames([]uintptr{pc})
	frame, _ := frames.Next()
	return fmt.Sprintf("%s:%d", trimSourceFile(frame.File), frame.Line)
}

func trimSourceFile(file string) string {
	if file == "" {
		return ""
	}
	if rel, ok := trimSourceFileByProjectRoot(file); ok {
		return rel
	}

	normalized := filepath.ToSlash(file)
	rootName := sourceModuleDirectoryName()
	if rootName == "" {
		return normalized
	}
	rootMarker := "/" + rootName + "/"
	if index := strings.LastIndex(normalized, rootMarker); index >= 0 {
		return normalized[index+len(rootMarker):]
	}
	if strings.HasPrefix(normalized, rootName+"/") {
		return strings.TrimPrefix(normalized, rootName+"/")
	}
	return normalized
}

func trimSourceFileByProjectRoot(file string) (string, bool) {
	root := sourceRootDirectory()
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", false
	}
	return rel, true
}

func sourceRootDirectory() string {
	sourceRootMu.Lock()
	defer sourceRootMu.Unlock()
	if sourceRootDir != "" {
		return sourceRootDir
	}
	root := findSourceRootDirectory()
	if root != "" {
		sourceRootDir = root
	}
	return root
}

func findSourceRootDirectory() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return ""
		}
		wd = parent
	}
}

func sourceModuleDirectoryName() string {
	sourceModuleNameOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		sourceModuleName = moduleDirectoryName(info.Main.Path)
	})
	return sourceModuleName
}

func moduleDirectoryName(modulePath string) string {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" || modulePath == "command-line-arguments" {
		return ""
	}
	name := path.Base(modulePath)
	if name == "." || name == "/" {
		return ""
	}
	return name
}
