import { describe, expect, test } from "bun:test";
import { qrImageSrc } from "./totp-qr";

describe("qrImageSrc", () => {
  test("keeps a PNG data URL", () => {
    expect(qrImageSrc("data:image/png;base64,iVBORw0KGgo=")).toBe(
      "data:image/png;base64,iVBORw0KGgo=",
    );
  });

  test("refuses the otpauth provisioning URL", () => {
    expect(qrImageSrc("otpauth://totp/Phoenix:alice?secret=ABC&issuer=Phoenix")).toBe("");
  });

  test("refuses a missing value from an older hub", () => {
    expect(qrImageSrc(undefined)).toBe("");
    expect(qrImageSrc("")).toBe("");
  });
});
