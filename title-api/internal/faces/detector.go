// Package faces tells who of a title's cast is on the screen: on a pause,
// the faces of the paused frame are found and made into vectors by the
// faces service (faces/ in the repository) and told apart by those of the
// people's photos on TMDB, the references, which are made in the
// background for the people of the episodes played. Each face seen is kept
// with what it was taken for and a picture of it, for the clusters of a
// series' faces that will make references of their own.
package faces

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// dimensions of a face's vector, and the model that makes them, the
// faces service's: vectors of another model don't compare.
const (
	dimensions = 512
	model      = "w600k_r50"
)

// face is a face the faces service found in a picture: its box and five
// points (eyes, nose, mouth corners) in the picture's pixels, how sure
// the detector is, how far it is turned (the nose's distance from between
// the eyes, in eye distances), how sharp it is (the variance of the
// Laplacian of the aligned face) and its vector, of unit length; one too
// small has none.
type face struct {
	Box       [4]float64    `json:"box"`
	Points    [5][2]float64 `json:"kps"`
	Det       float64       `json:"det"`
	Yaw       float64       `json:"yaw"`
	Blur      float64       `json:"blur"`
	Embedding vector        `json:"embedding"`
}

func (f face) width() float64  { return f.Box[2] - f.Box[0] }
func (f face) height() float64 { return f.Box[3] - f.Box[1] }
func (f face) side() float64   { return math.Min(f.width(), f.height()) }
func (f face) area() float64   { return f.width() * f.height() }

type detection struct {
	Model  string `json:"model"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Faces  []face `json:"faces"`
	Ms     int    `json:"ms"`
}

// vector is a face's vector; in JSON, its float32s little-endian in
// base64, as the faces service sends them.
type vector []float32

func (v *vector) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return err
	}
	return v.decode(raw)
}

func (v *vector) decode(raw []byte) error {
	if len(raw) != dimensions*4 {
		return fmt.Errorf("a face's vector of %d bytes", len(raw))
	}
	*v = make(vector, dimensions)
	for i := range *v {
		(*v)[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return nil
}

func (v vector) bytes() []byte {
	raw := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(x))
	}
	return raw
}

func (v vector) dot(w vector) float64 {
	var sum float64
	for i := range v {
		sum += float64(v[i]) * float64(w[i])
	}
	return sum
}

// mean is the mean of vectors, made of unit length again.
func mean(vectors []vector) vector {
	if len(vectors) == 0 {
		return nil
	}
	sum := make([]float64, dimensions)
	for _, v := range vectors {
		for i, x := range v {
			sum[i] += float64(x)
		}
	}
	var norm float64
	for _, x := range sum {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	result := make(vector, dimensions)
	for i, x := range sum {
		result[i] = float32(x / norm)
	}
	return result
}

// detector asks the faces service.
type detector struct {
	url  string
	http *http.Client
}

func newDetector(url string) *detector {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &detector{url: url, http: &http.Client{Transport: transport, Timeout: time.Minute}}
}

// look is how the faces service is to look at a picture: made size
// pixels on its longer side for the detector (320, 480, 640 or 800), the
// vectors of the most largest faces of minSide pixels or more on their
// shorter side, the others with none: a vector takes a second on the
// server.
type look struct {
	size, minSide, most int
}

// detect finds the faces in a JPEG or a PNG, the largest first.
func (d *detector) detect(ctx context.Context, picture []byte, l look) (detection, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/detect?det=%d&min=%d&max=%d", d.url, l.size, l.minSide, l.most), bytes.NewReader(picture))
	if err != nil {
		return detection{}, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := d.http.Do(request)
	if err != nil {
		return detection{}, fmt.Errorf("faces service: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return detection{}, fmt.Errorf("faces service: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return detection{}, fmt.Errorf("faces service: %s: %s", response.Status, bytes.TrimSpace(body))
	}
	var result detection
	if err := json.Unmarshal(body, &result); err != nil {
		return detection{}, fmt.Errorf("faces service: %w", err)
	}
	return result, nil
}
