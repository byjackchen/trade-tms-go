/**
 * (e) Auth: a direct API call without a token is rejected with 401.
 *
 * Per docs/api.md every /api/* route requires `Authorization: Bearer <token>`;
 * a missing/invalid token returns 401 with the standard error envelope
 * (`code: "unauthorized"`) and a `WWW-Authenticate: Bearer realm="tms-api"`
 * challenge. `/healthz` is public. The token is never echoed in logs/headers.
 */

import { test, expect } from "../fixtures/test";
import { getNoAuth, getAuthed, getHealthz } from "../lib/api";

test.describe("API authentication", () => {
  test("unauthenticated /api/v1 call is 401 with the unauthorized envelope", async () => {
    const res = await getNoAuth("data/coverage");
    expect(res.status, "missing bearer token must be rejected").toBe(401);

    // Standard error envelope with code "unauthorized".
    expect(res.body).toMatchObject({
      error: { code: "unauthorized" },
    });

    // Bearer challenge header present.
    const challenge = res.headers.get("www-authenticate");
    expect(challenge, "WWW-Authenticate challenge present").toBeTruthy();
    expect(challenge ?? "").toContain("Bearer");
  });

  test("a bogus token is also 401", async () => {
    const res = await fetch(
      `${process.env.TMS_E2E_API_URL ?? "http://localhost:18080"}/api/v1/jobs`,
      { headers: { Authorization: "Bearer definitely-not-the-token" } },
    );
    expect(res.status).toBe(401);
  });

  test("the correct token is accepted (200) on a read endpoint", async () => {
    const res = await getAuthed("data/coverage");
    expect(
      res.status,
      "valid bearer token should be accepted",
    ).toBe(200);
  });

  test("/healthz is public (no token required)", async () => {
    const res = await getHealthz();
    expect(res.status).toBe(200);
    expect(res.body).toMatchObject({ status: expect.any(String) });
  });
});
