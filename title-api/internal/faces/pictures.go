package faces

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
)

// The pictures a pause keeps: its frame, its longer side frameSide pixels
// at most, and each face, a square of cropSide pixels around it, its box
// grown cropGrow times so that the hair and the shoulders are in.
const (
	frameSide = 1280
	cropSide  = 256
	cropGrow  = 1.8
)

func decodeJPEG(data []byte) (*image.RGBA, error) {
	picture, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	result := image.NewRGBA(picture.Bounds().Sub(picture.Bounds().Min))
	draw.Draw(result, result.Bounds(), picture, picture.Bounds().Min, draw.Src)
	return result, nil
}

func encodeJPEG(picture image.Image, quality int) []byte {
	var buffer bytes.Buffer
	_ = jpeg.Encode(&buffer, picture, &jpeg.Options{Quality: quality})
	return buffer.Bytes()
}

// scaled is the frame with its longer side frameSide at most, and the
// scale of it.
func scaled(picture *image.RGBA) (*image.RGBA, float64) {
	w, h := picture.Bounds().Dx(), picture.Bounds().Dy()
	scale := math.Min(1, float64(frameSide)/float64(max(w, h)))
	if scale == 1 {
		return picture, 1
	}
	return resample(picture, 0, 0, float64(w), float64(h), int(math.Round(float64(w)*scale)), int(math.Round(float64(h)*scale))), scale
}

// crop is a face's square of the frame, cropSide pixels, the frame's edges
// cutting it.
func crop(picture *image.RGBA, box [4]float64) *image.RGBA {
	cx, cy := (box[0]+box[2])/2, (box[1]+box[3])/2
	side := math.Max(box[2]-box[0], box[3]-box[1]) * cropGrow
	w, h := float64(picture.Bounds().Dx()), float64(picture.Bounds().Dy())
	side = math.Min(side, math.Min(w, h))
	x0 := math.Min(math.Max(cx-side/2, 0), w-side)
	// A little above the middle: more of the hair than of the neck.
	y0 := math.Min(math.Max(cy-side*0.45, 0), h-side)
	return resample(picture, x0, y0, side, side, cropSide, cropSide)
}

// resample is the rectangle (x, y, w, h) of src made dw×dh pixels: each
// pixel the mean of those it covers when made smaller, bilinear when made
// larger.
func resample(src *image.RGBA, x, y, w, h float64, dw, dh int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	sx, sy := w/float64(dw), h/float64(dh)
	maxX, maxY := src.Bounds().Dx()-1, src.Bounds().Dy()-1
	at := func(px, py int) (float64, float64, float64) {
		px, py = min(max(px, 0), maxX), min(max(py, 0), maxY)
		i := src.PixOffset(px, py)
		return float64(src.Pix[i]), float64(src.Pix[i+1]), float64(src.Pix[i+2])
	}
	for j := range dh {
		for i := range dw {
			var r, g, b float64
			if sx > 1 || sy > 1 {
				x0, x1 := int(x+float64(i)*sx), int(math.Ceil(x+float64(i+1)*sx))
				y0, y1 := int(y+float64(j)*sy), int(math.Ceil(y+float64(j+1)*sy))
				n := 0.0
				for py := y0; py < max(y1, y0+1); py++ {
					for px := x0; px < max(x1, x0+1); px++ {
						pr, pg, pb := at(px, py)
						r, g, b, n = r+pr, g+pg, b+pb, n+1
					}
				}
				r, g, b = r/n, g/n, b/n
			} else {
				fx, fy := x+(float64(i)+0.5)*sx-0.5, y+(float64(j)+0.5)*sy-0.5
				ix, iy := int(math.Floor(fx)), int(math.Floor(fy))
				tx, ty := fx-float64(ix), fy-float64(iy)
				r00, g00, b00 := at(ix, iy)
				r10, g10, b10 := at(ix+1, iy)
				r01, g01, b01 := at(ix, iy+1)
				r11, g11, b11 := at(ix+1, iy+1)
				mix := func(a, b, c, d float64) float64 {
					return (a*(1-tx)+b*tx)*(1-ty) + (c*(1-tx)+d*tx)*ty
				}
				r, g, b = mix(r00, r10, r01, r11), mix(g00, g10, g01, g11), mix(b00, b10, b01, b11)
			}
			dst.SetRGBA(i, j, color.RGBA{uint8(r + 0.5), uint8(g + 0.5), uint8(b + 0.5), 255})
		}
	}
	return dst
}
