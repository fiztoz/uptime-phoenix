/**
 * Returns the setup-2fa QR code if it is a PNG data: URL, which the CSP
 * img-src allows, and "" otherwise. Never pass the otpauth:// URL to
 * <img src>: browsers cannot load it, the CSP blocks it, and the blocked
 * request logs the URL, secret included, to the console (#121).
 */
export function qrImageSrc(qrImage: string | null | undefined): string {
  return typeof qrImage === "string" &&
    qrImage.startsWith("data:image/png;base64,")
    ? qrImage
    : "";
}
