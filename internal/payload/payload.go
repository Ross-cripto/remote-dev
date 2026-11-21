package payload

import (
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"regexp"
	"strings"
)

//go:embed assets/*
var files embed.FS

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func name(architecture string) (string, error) {
	switch architecture {
	case "amd64", "arm64":
		return "assets/linux-" + architecture, nil
	default:
		return "", fmt.Errorf("unsupported helper architecture %q", architecture)
	}
}

func Digest(architecture string) (string, error) {
	path, err := name(architecture)
	if err != nil {
		return "", err
	}
	content, err := files.ReadFile(path + ".sha256")
	if err != nil {
		return "", fmt.Errorf("the Linux %s helper is missing from this client build. Build with scripts/build-native.sh", architecture)
	}
	digest := strings.TrimSpace(string(content))
	if !sha256Pattern.MatchString(digest) {
		return "", fmt.Errorf("invalid bundled Linux %s helper digest", architecture)
	}
	return digest, nil
}

type archive struct {
	io.Reader
	file io.Closer
	gzip io.Closer
}

func (reader archive) Close() error {
	defer reader.file.Close()
	return reader.gzip.Close()
}

func Open(architecture string) (io.ReadCloser, error) {
	path, err := name(architecture)
	if err != nil {
		return nil, err
	}
	file, err := files.Open(path + ".gz")
	if err != nil {
		return nil, err
	}
	expanded, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	return archive{Reader: expanded, file: file, gzip: expanded}, nil
}
