package handlers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"

	"github.com/pquerna/otp"
)

// qrImageSize is the edge length, in pixels, of the setup-2fa QR code.
const qrImageSize = 256

// qrPNGDataURL renders an otpauth:// provisioning URL as a PNG QR code
// and returns it as a data: URL. The browser cannot load otpauth:// as
// an image, and the CSP img-src only allows 'self', data: and blob:
// (plus https: in production), so the QR code has to travel as data.
func qrPNGDataURL(otpauthURL string) (string, error) {
	key, err := otp.NewKeyFromURL(otpauthURL)
	if err != nil {
		return "", fmt.Errorf("parse provisioning url: %w", err)
	}
	img, err := key.Image(qrImageSize, qrImageSize)
	if err != nil {
		return "", fmt.Errorf("render qr code: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("encode qr code: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
