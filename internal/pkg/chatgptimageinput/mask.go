package imageinput

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
)

// CompositeMasks applies edit masks to source images. A transparent mask
// pixel marks the editable area, matching the Python implementation: RGBA
// masks contribute alpha, while non-alpha masks contribute luminance. A lone
// mask applies to every image; otherwise masks are paired by index and the
// final mask is reused for trailing images.
func CompositeMasks(images, masks [][]byte) ([][]byte, error) {
	if len(masks) == 0 {
		return images, nil
	}
	result := make([][]byte, 0, len(images))
	for index, source := range images {
		maskIndex := min(index, len(masks)-1)
		mask := masks[maskIndex]
		composited, err := compositeMask(source, mask)
		if err != nil {
			return nil, fmt.Errorf("image %d: %w", index+1, err)
		}
		result = append(result, composited)
	}
	return result, nil
}

func compositeMask(source, mask []byte) ([]byte, error) {
	base, _, err := image.Decode(bytes.NewReader(source))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	maskImage, _, err := image.Decode(bytes.NewReader(mask))
	if err != nil {
		return nil, fmt.Errorf("decode mask: %w", err)
	}
	bounds, maskBounds := base.Bounds(), maskImage.Bounds()
	if bounds.Empty() || maskBounds.Empty() {
		return nil, fmt.Errorf("empty image or mask")
	}
	output := image.NewNRGBA(bounds)
	draw.Draw(output, bounds, base, bounds.Min, draw.Src)
	maskHasAlpha := imageHasAlpha(maskImage)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			mx := maskBounds.Min.X + (x-bounds.Min.X)*maskBounds.Dx()/bounds.Dx()
			my := maskBounds.Min.Y + (y-bounds.Min.Y)*maskBounds.Dy()/bounds.Dy()
			r, g, b, a := maskImage.At(mx, my).RGBA()
			alpha := uint8(a >> 8)
			if !maskHasAlpha {
				alpha = uint8(((299*r + 587*g + 114*b) / 1000) >> 8)
			}
			original := output.NRGBAAt(x, y)
			output.SetNRGBA(x, y, color.NRGBA{R: original.R, G: original.G, B: original.B, A: alpha})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, output); err != nil {
		return nil, fmt.Errorf("encode composited image: %w", err)
	}
	return encoded.Bytes(), nil
}

func imageHasAlpha(value image.Image) bool {
	switch value.(type) {
	case *image.Alpha, *image.Alpha16, *image.NRGBA, *image.NRGBA64, *image.RGBA, *image.RGBA64, *image.NYCbCrA:
		return true
	default:
		return false
	}
}
