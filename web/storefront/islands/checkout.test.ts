import { expect, test } from "bun:test";
import { answersFor, type CheckoutData, fieldErrors, PENDING, payload, waitStep } from "./checkout";
import { eventPath, orderPath } from "./links";

type Question = CheckoutData["questions"][number];

const q = (id: string, over: Partial<Question>): Question => ({
  id,
  label: id,
  kind: "text",
  options: [],
  required: false,
  scope: "order",
  applies_to_all: true,
  applies_to: [],
  ...over,
});

const ORDER = q("q-order", { label: "Dietary needs" });
const VIP_ONLY = q("q-vip", {
  label: "Plus-one",
  scope: "ticket",
  applies_to_all: false,
  applies_to: ["vip"],
});
const AGREE = q("q-agree", { kind: "checkbox", label: "Photos OK" });
const items = [
  { ticket_type_id: "general", quantity: 2 },
  { ticket_type_id: "vip", quantity: 1 },
];

test("one field per order question and per ticket a ticket question asks", () => {
  const fields = answersFor([ORDER, VIP_ONLY], items);
  expect(fields.map((f) => [f.question.id, f.ticket_index, f.name])).toEqual([
    ["q-order", -1, "answers.q-order.-1"],
    ["q-vip", 2, "answers.q-vip.2"],
  ]);
});

test("an order question asked only of types not in the cart is skipped", () => {
  const vipOrder = q("q-x", { applies_to_all: false, applies_to: ["vip"] });
  expect(answersFor([vipOrder], [{ ticket_type_id: "general", quantity: 1 }])).toEqual([]);
});

test("payload maps names and answers; an unticked checkbox is false", () => {
  const fields = answersFor([ORDER, VIP_ONLY, AGREE], items);
  const form = new FormData();
  form.set("email", "ada@example.com");
  for (const n of ["Ada", "Grace", "Lin"]) form.append("attendee", n);
  form.set("answers.q-order.-1", "None");
  form.set("answers.q-vip.2", "Sam");
  expect(payload(form, fields)).toEqual({
    email: "ada@example.com",
    attendees: ["Ada", "Grace", "Lin"],
    answers: [
      { question_id: "q-order", ticket_index: -1, value: "None" },
      { question_id: "q-vip", ticket_index: 2, value: "Sam" },
      { question_id: "q-agree", ticket_index: -1, value: "false" },
    ],
  });
  form.set("answers.q-agree.-1", "true");
  expect(payload(form, fields).answers?.[2]?.value).toBe("true");
});

test("422 errors land on the input they name, in our words", () => {
  expect(fieldErrors({ code: "bad_email", field: "email" })).toEqual({
    email: "That email doesn't look right. It should look like ada@example.com.",
  });
  expect(fieldErrors({ code: "missing_answer", field: "answers.q-order.-1" })).toEqual({
    "answers.q-order.-1": "This one needs an answer.",
  });
  expect(
    fieldErrors({ code: "bad_answer", field: "answers.q-vip.2", message: "<b>API</b>" }),
  ).toEqual({
    "answers.q-vip.2": "That answer doesn't fit this question. Check it and try again.",
  });
  expect(fieldErrors({ code: "price_changed" })).toEqual({});
});

test("the way back is always a path on this site, and needs both slugs", () => {
  expect(eventPath({ organizer: "acme", event_slug: "gala" })).toBe("/acme/gala");
  expect(eventPath({ organizer: "", event_slug: "gala" })).toBeUndefined();
  expect(eventPath({ organizer: "acme", event_slug: "" })).toBeUndefined();
  expect(eventPath({ organizer: "javascript:alert(1)//", event_slug: "/evil.example" })).toBe(
    "/javascript%3Aalert(1)%2F%2F/%2Fevil.example",
  );
});

const T = "q3Jx0_Vb-8sKd2LmN4pR7tWzYcE1gH5iA9oU6fXnBvM";

test("an order link is rebuilt from its code and token, never its path", () => {
  expect(orderPath(`http://localhost:3003/orders/TS-7K2QF?t=${T}`)).toBe(`/orders/TS-7K2QF?t=${T}`);
  expect(orderPath(`https://shop.example/orders/TS-7K2QF?t=${T}`, "TS-7K2QF")).toBe(
    `/orders/TS-7K2QF?t=${T}`,
  );
  for (const bad of [
    `http://localhost:3003//evil.example/orders/TS-7K2QF?t=${T}`,
    `http://localhost:3003/\\evil.example/orders/TS-7K2QF?t=${T}`,
    `//evil.example/orders/TS-7K2QF?t=${T}`,
  ]) {
    const got = orderPath(bad);
    // Either refused, or rebuilt as our own /orders path.
    if (got !== undefined) expect(got).toBe(`/orders/TS-7K2QF?t=${T}`);
  }
  expect(orderPath(`/orders/TS-7K2QF?t=short`)).toBeUndefined();
  expect(orderPath(`/orders/TS-ILOU0?t=${T}`)).toBeUndefined();
  expect(orderPath(`/orders/TS-7K2QF?t=${T}`, "TS-ABCDE")).toBeUndefined();
  expect(orderPath("not a url at all")).toBeUndefined();
});

test("while a payment may be going through, a lapsed hold never reads as back on sale", () => {
  const now = Date.parse("2027-05-02T01:20:00Z");
  const past = "2027-05-02T01:15:00Z";
  const ahead = "2027-05-02T01:25:00Z";
  expect(waitStep({ status: "completed", expires_at: past }, true, now)).toBe("done");
  expect(waitStep({ status: "paying", expires_at: ahead }, true, now)).toBe("wait");
  // The hold ran out after the card went in: keep waiting, with the pending words.
  expect(waitStep({ status: "paying", expires_at: past }, true, now)).toBe("late");
  expect(waitStep({ status: "paying", expires_at: past }, false, now)).toBe("late");
  // We started a payment on this page: even "expired" can't mean back on sale.
  expect(waitStep({ status: "expired", expires_at: past }, true, now)).toBe("late");
  // Nothing started here, and the API says no payment exists: the hold really is over.
  expect(waitStep({ status: "expired", expires_at: past }, false, now)).toBe("over");
  expect(PENDING).toBe(
    "Your payment may still be going through. We'll email you the moment it does. Check your inbox before you try again.",
  );
  expect(PENDING).not.toContain("back on sale");
});
