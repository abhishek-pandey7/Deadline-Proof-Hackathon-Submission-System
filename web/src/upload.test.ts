import { describe, expect, it } from "vitest";
import { ApiError } from "./api";
import { backoffMs, batches, isFatal, missingParts, partRange } from "./upload";

describe("upload planning", () => {
  it("sends only the parts storage does not have", () => {
    expect(missingParts(5, [{ number: 1 }, { number: 4 }])).toEqual([2, 3, 5]);
    expect(missingParts(3, [{ number: 1 }, { number: 2 }, { number: 3 }])).toEqual([]);
  });

  it("splits the file into fixed parts with a shorter last part", () => {
    const part = 8 << 20;
    const size = 100 << 20;
    expect(partRange(1, part, size)).toEqual([0, part]);
    expect(partRange(13, part, size)).toEqual([12 * part, size]);
    let covered = 0;
    for (let n = 1; n <= 13; n++) {
      const [a, b] = partRange(n, part, size);
      covered += b - a;
    }
    expect(covered).toBe(size);
  });

  it("signs at most 20 parts per request", () => {
    expect(batches([...Array(45).keys()], 20).map((b) => b.length)).toEqual([20, 20, 5]);
  });

  it("backs off but never waits more than about 15 seconds", () => {
    for (let i = 0; i < 10; i++) expect(backoffMs(i)).toBeLessThanOrEqual(15_000 * 1.25);
    expect(backoffMs(0)).toBeGreaterThanOrEqual(750);
  });

  it("stops on rules, keeps trying on overload and gaps", () => {
    expect(isFatal(new ApiError(409, "grace_over", ""))).toBe(true);
    expect(isFatal(new ApiError(409, "deadline_passed", ""))).toBe(true);
    expect(isFatal(new ApiError(503, "storage_unavailable", ""))).toBe(false);
    expect(isFatal(new ApiError(429, "rate_limited", ""))).toBe(false);
    expect(isFatal(new ApiError(409, "parts_missing", ""))).toBe(false);
    expect(isFatal(new TypeError("network"))).toBe(false);
  });
});
