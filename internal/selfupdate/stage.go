package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/z2z23n0/tooltend/internal/safeio"
)

type Fetcher interface {
	Fetch(context.Context, string, int64) ([]byte, error)
}

type HTTPFetcher struct {
	Client *http.Client
}

func (f HTTPFetcher) Fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}
	reader := io.LimitReader(response.Body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("download exceeds signed size limit")
	}
	return data, nil
}

func Stage(ctx context.Context, fetcher Fetcher, verified Verified, stateDir string) (string, error) {
	if fetcher == nil {
		return "", errors.New("fetcher is required")
	}
	if !safeVersion(verified.Manifest.Version) {
		return "", errors.New("verified release version is unsafe for a file name")
	}
	data, err := fetcher.Fetch(ctx, verified.Asset.URL, verified.Asset.Size)
	if err != nil {
		return "", err
	}
	if err := VerifyAsset(data, verified.Asset); err != nil {
		return "", err
	}
	pendingDir := filepath.Join(stateDir, "self-update")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(pendingDir, "tooltend-"+verified.Manifest.Version+".pending")
	if err := safeio.AtomicWriteFile(path, data, 0o700); err != nil {
		return "", err
	}
	return path, nil
}

func safeVersion(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '+' || char == '-' {
			continue
		}
		return false
	}
	return true
}
