/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import { customStatusPageIcon } from "./status-page-icon";

describe("customStatusPageIcon", () => {
	test("treats empty as the Phoenix mascot default", () => {
		expect(customStatusPageIcon(undefined)).toBeNull();
		expect(customStatusPageIcon("")).toBeNull();
		expect(customStatusPageIcon("   ")).toBeNull();
	});

	test("treats Kuma default /icon.svg as unset", () => {
		expect(customStatusPageIcon("/icon.svg")).toBeNull();
		expect(customStatusPageIcon("/icon.png")).toBeNull();
		expect(customStatusPageIcon("https://phoenix.example/icon.svg")).toBeNull();
	});

	test("keeps a real custom logo", () => {
		expect(customStatusPageIcon("/brand/phoenix-mascot.svg")).toBe(
			"/brand/phoenix-mascot.svg",
		);
		expect(customStatusPageIcon("https://cdn.example/logo.png")).toBe(
			"https://cdn.example/logo.png",
		);
		expect(customStatusPageIcon("data:image/png;base64,abc")).toBe(
			"data:image/png;base64,abc",
		);
	});
});
