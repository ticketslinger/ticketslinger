import { expect, test } from "bun:test";
import { type AddOrderForm, addOrderBody, addOrderProblem, soldOutText } from "./AddOrder";

const form = (over: Partial<AddOrderForm> = {}): AddOrderForm => ({
  kind: "comp",
  email: " guest@example.com ",
  buyerName: "",
  lines: [
    { ticket_type_id: "t1", quantity: "2" },
    { ticket_type_id: "t2", quantity: "" },
  ],
  names: "",
  method: "cash",
  amount: "",
  sendTickets: true,
  note: "",
  ...over,
});

test("a comp sends no method or amount", () => {
  const b = addOrderBody(form());
  expect(b).toEqual({
    kind: "comp",
    email: "guest@example.com",
    lines: [{ ticket_type_id: "t1", quantity: 2 }],
    send_tickets: true,
  });
});

test("a manual order sends its method and the amount in cents", () => {
  const b = addOrderBody(
    form({
      kind: "manual",
      amount: "$10.01",
      method: "invoice",
      buyerName: " Ada ",
      names: "Ada\nGrace\n\n",
      note: "Paid at the door",
      sendTickets: false,
    }),
  );
  expect(b).toEqual({
    kind: "manual",
    email: "guest@example.com",
    buyer_name: "Ada",
    lines: [{ ticket_type_id: "t1", quantity: 2 }],
    attendees: ["Ada", "Grace"],
    method: "invoice",
    amount_cents: 1001,
    send_tickets: false,
    note: "Paid at the door",
  });
});

test("a manual order with no amount asks for one", () => {
  expect(addOrderProblem(form({ kind: "manual", amount: "" }))).toEqual({
    field: "amount_cents",
    message: "Enter what they paid, or $0.",
  });
  expect(addOrderProblem(form({ kind: "manual", amount: "0" }))).toBeNull();
  expect(addOrderProblem(form())).toBeNull();
});

test("sold out names each ticket that's short", () => {
  const types = [
    { id: "t1", name: "General" },
    { id: "t2", name: "VIP" },
  ];
  const f = form({
    lines: [
      { ticket_type_id: "t1", quantity: "3" },
      { ticket_type_id: "t2", quantity: "1" },
    ],
  });
  expect(
    soldOutText(f, types, [
      { ticket_type_id: "t1", remaining: 1 },
      { ticket_type_id: "t2", remaining: 5 },
    ]),
  ).toBe("Only 1 General left.");
});
