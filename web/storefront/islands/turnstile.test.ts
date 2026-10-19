import { expect, test } from "bun:test";
import { lazyCheck, type Turnstile } from "./turnstile";

/** A ticket form without a DOM: events, the widget box, the token field and requestSubmit. */
function fakeForm(token = "") {
  const field = { value: token };
  const box = { dataset: { sitekey: "1x00000000000000000000AA" } };
  const form = Object.assign(new EventTarget(), {
    submitted: 0,
    querySelector: (sel: string) => (sel === ".cf-turnstile" ? box : field),
    requestSubmit() {
      form.submitted++;
    },
  });
  return { form, field, box };
}

/** A load() that counts calls and hands back a widget whose check passes when `pass()` is called. */
function fakeLoad(fail = false) {
  const calls = { load: 0, render: [] as unknown[] };
  let pass = () => {};
  const turnstile: Turnstile = {
    render(el, o) {
      calls.render.push([el, o.sitekey]);
      pass = () => o.callback("tok");
    },
  };
  const load = () => {
    calls.load++;
    return fail ? Promise.reject(new Error("blocked")) : Promise.resolve(turnstile);
  };
  return { calls, load, pass: () => pass() };
}

const tick = () => new Promise((r) => setTimeout(r, 0));
const submit = () => new Event("submit", { cancelable: true });

test("Turnstile waits for the buyer to start on the form, and loads once", async () => {
  const { form, box } = fakeForm();
  const { calls, load } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  await tick();
  expect(calls.load).toBe(0);
  for (const type of ["focusin", "change", "pointerdown"]) form.dispatchEvent(new Event(type));
  await tick();
  expect(calls.load).toBe(1);
  expect(calls.render).toEqual([[box, "1x00000000000000000000AA"]]);
});

for (const type of ["focusin", "change", "pointerdown"]) {
  test(`${type} on the form starts the check`, async () => {
    const { form } = fakeForm();
    const { calls, load } = fakeLoad();
    lazyCheck(form as unknown as HTMLFormElement, load);
    form.dispatchEvent(new Event(type));
    await tick();
    expect(calls.load).toBe(1);
  });
}

test("a submit before the check holds the form, then sends it once the token arrives", async () => {
  const { form, field } = fakeForm();
  const { calls, load, pass } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  const e = submit();
  form.dispatchEvent(e);
  expect(e.defaultPrevented).toBe(true);
  await tick();
  expect(calls.load).toBe(1);
  expect(form.submitted).toBe(0);
  field.value = "tok";
  pass();
  await tick();
  expect(form.submitted).toBe(1);
});

test("a submit with a token goes straight through", () => {
  const { form } = fakeForm("tok");
  const { load } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  const e = submit();
  form.dispatchEvent(e);
  expect(e.defaultPrevented).toBe(false);
});

test("if Turnstile can't load, the form still goes, and the server asks for the check", async () => {
  const { form } = fakeForm();
  const { load } = fakeLoad(true);
  lazyCheck(form as unknown as HTMLFormElement, load);
  form.dispatchEvent(submit());
  await tick();
  expect(form.submitted).toBe(1);
  const again = submit();
  form.dispatchEvent(again);
  expect(again.defaultPrevented).toBe(false);
});
