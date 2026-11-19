package faces

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Pictures of the pauses kept at most: the frames of the latest
// framesKept, the faces taking cropsLimit bytes, the oldest gone first
// unless they are someone's portrait.
const (
	framesKept = 300
	cropsLimit = 300 << 20
)

// pictures keeps the pictures of the pauses as files: <dir>/frames/<pause
// ID>.jpg and <dir>/crops/<face ID>.jpg.
type pictures struct {
	dir string
}

func openPictures(dir string) (*pictures, error) {
	for _, kind := range []string{framePath, cropPath} {
		if err := os.MkdirAll(filepath.Join(dir, kind), 0o755); err != nil {
			return nil, err
		}
	}
	return &pictures{dir: dir}, nil
}

func (p *pictures) path(kind string, id int64) string {
	return filepath.Join(p.dir, kind, strconv.FormatInt(id, 10)+".jpg")
}

func (p *pictures) put(kind string, id int64, data []byte) error {
	path := p.path(kind, id)
	temporary := path + ".part"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func (p *pictures) get(kind string, id int64) ([]byte, error) {
	return os.ReadFile(p.path(kind, id))
}

type pictureFile struct {
	id   int64
	path string
	size int64
}

func (p *pictures) list(kind string) ([]pictureFile, error) {
	entries, err := os.ReadDir(filepath.Join(p.dir, kind))
	if err != nil {
		return nil, err
	}
	var result []pictureFile
	for _, entry := range entries {
		id, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".jpg"), 10, 64)
		if err != nil || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		result = append(result, pictureFile{id: id, path: filepath.Join(p.dir, kind, entry.Name()), size: info.Size()})
	}
	// IDs grow with time: the oldest first.
	sort.Slice(result, func(i, j int) bool { return result[i].id < result[j].id })
	return result, nil
}

// trim removes the frames beyond the latest framesKept and the oldest
// faces beyond cropsLimit, but those kept.
func (p *pictures) trim(kept map[int64]bool) error {
	frames, err := p.list(framePath)
	if err != nil {
		return err
	}
	for i := 0; i < len(frames)-framesKept; i++ {
		_ = os.Remove(frames[i].path)
	}
	crops, err := p.list(cropPath)
	if err != nil {
		return err
	}
	var total int64
	for _, c := range crops {
		total += c.size
	}
	for _, c := range crops {
		if total <= cropsLimit {
			break
		}
		if !kept[c.id] {
			_ = os.Remove(c.path)
			total -= c.size
		}
	}
	return nil
}
