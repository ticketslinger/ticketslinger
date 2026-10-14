import { expect, test } from "bun:test";
import { type ApiFailure, must } from "./api";
import { formField } from "./forms";
import { promoFields } from "./routes/Promos";
import { questionFields } from "./routes/Questions";
import { ticketFields } from "./routes/Tickets";

test("an API property finds its form field", () => {
  expect(formField("price_cents", ticketFields)).toBe("price");
  expect(formField("quota_ids", ticketFields)).toBe("choices");
  expect(formField("valid_until", promoFields)).toBe("until");
  expect(formField("applies_to", promoFields)).toBe("types");
  expect(formField("code", promoFields)).toBe("code");
  expect(formField("label", questionFields)).toBe("label");
});

test("an option's index is dropped, the options field is marked", () => {
  expect(formField("options[2]", questionFields)).toBe("options");
  expect(formField("options", questionFields)).toBe("options");
});

test("no property, or one this form doesn't have, means the form as a whole", () => {
  expect(formField("", ticketFields)).toBe("");
  expect(formField("slug", ticketFields)).toBe("");
  expect(formField("constructor", ticketFields)).toBe("");
});

test("a failed response keeps the field the API named", () => {
  const res = (error: unknown) => ({ error, response: new Response(null, { status: 422 }) });
  const caught = (error: unknown) => {
    try {
      must(res(error));
    } catch (e) {
      return e as ApiFailure;
    }
    throw new Error("must didn't throw");
  };
  const named = caught({ error: { code: "bad_price", message: "No.", field: "price_cents" } });
  expect([named.code, named.field]).toEqual(["bad_price", "price_cents"]);
  expect(caught({ error: { code: "bad_kind", message: "No." } }).field).toBe("");
});
