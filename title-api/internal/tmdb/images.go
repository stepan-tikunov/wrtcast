package tmdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const imagesURL = "https://image.tmdb.org/t/p"

var (
	// ErrInvalidImage is a size or a file /v1/tmdb/images doesn't serve.
	ErrInvalidImage = errors.New("invalid TMDB image")
	// ErrImageNotFound is a picture TMDB doesn't have.
	ErrImageNotFound = errors.New("TMDB has no such image")
)

var (
	imageSizes = []string{"w185", "w300", "w342", "w500", "w780", "w1280"}
	imageFile  = regexp.MustCompile(`^[A-Za-z0-9]{10,64}\.(jpg|png)$`)
)

// Images serves TMDB's pictures, kept on disk while they take no more
// than a limit, the least recently served removed first. A picture's path
// on TMDB never changes what it is.
type Images struct {
	dir     string
	limit   int64
	http    *http.Client
	baseURL string

	mu      sync.Mutex
	flights map[string]*imageFlight
}

type imageFlight struct {
	done chan struct{}
	data []byte
	err  error
}

func NewImages(dir string, limit int64) (*Images, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Images{
		dir: dir, limit: limit, http: &http.Client{Transport: transport, Timeout: time.Minute},
		baseURL: imagesURL, flights: map[string]*imageFlight{},
	}, nil
}

// Run trims the pictures kept now and every hour, until ctx is done.
func (i *Images) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		if err := i.trim(); err != nil {
			log.Printf("tmdb images: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Image is a picture of a size, and its type.
func (i *Images) Image(ctx context.Context, size, name string) ([]byte, string, error) {
	if !slices.Contains(imageSizes, size) || !imageFile.MatchString(name) {
		return nil, "", ErrInvalidImage
	}
	contentType := "image/jpeg"
	if strings.HasSuffix(name, ".png") {
		contentType = "image/png"
	}
	path := filepath.Join(i.dir, size+"_"+name)
	if data, err := os.ReadFile(path); err == nil {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
		return data, contentType, nil
	}
	f := i.fetch(size, name, path)
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case <-f.done:
	}
	return f.data, contentType, f.err
}

func (i *Images) fetch(size, name, path string) *imageFlight {
	key := size + "/" + name
	i.mu.Lock()
	defer i.mu.Unlock()
	if f := i.flights[key]; f != nil {
		return f
	}
	f := &imageFlight{done: make(chan struct{})}
	i.flights[key] = f
	go func() {
		f.data, f.err = i.download(key)
		if f.err == nil {
			if err := writeFile(path, f.data); err != nil {
				log.Printf("tmdb image %s: %v", key, err)
			}
		}
		i.mu.Lock()
		delete(i.flights, key)
		i.mu.Unlock()
		close(f.done)
	}()
	return f
}

func (i *Images) download(key string) ([]byte, error) {
	response, err := i.http.Get(i.baseURL + "/" + key)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, ErrImageNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB image %s: %s", key, response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, 16<<20))
}

// writeFile writes a file whole or not at all.
func writeFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(temp.Name())
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(temp.Name())
		return err
	}
	return os.Rename(temp.Name(), path)
}

// trim removes the pictures least recently served while they take more
// than the limit, down to nine tenths of it, and what writes left behind.
func (i *Images) trim() error {
	entries, err := os.ReadDir(i.dir)
	if err != nil {
		return err
	}
	type kept struct {
		path string
		size int64
		used time.Time
	}
	var files []kept
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(i.dir, entry.Name())
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			if time.Since(info.ModTime()) > time.Hour {
				os.Remove(path)
			}
			continue
		}
		files = append(files, kept{path, info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= i.limit {
		return nil
	}
	slices.SortFunc(files, func(a, b kept) int { return a.used.Compare(b.used) })
	for _, f := range files {
		if total <= i.limit*9/10 {
			break
		}
		if err := os.Remove(f.path); err != nil {
			return err
		}
		total -= f.size
	}
	return nil
}
