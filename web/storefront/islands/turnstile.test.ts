import { expect, test } from "bun:test";
import { lazyCheck, type Turnstile } from "./turnstile";

type Field = { value: string; type?: string; name?: string };

/** A ticket form without a DOM: events, the widget box, the token field and requestSubmit. */
function fakeForm(token: string | null = "") {
  // null: Turnstile never added its field.
  let field: Field | null = token === null ? null : { value: token };
  const box = { dataset: { sitekey: "1x00000000000000000000AA" } };
  // Like the browser's: requestSubmit fires submit, and only an unprevented one sends the form.
  // Capped, so a loop shows up as a count instead of a frozen test run.
  const view = new EventTarget();
  const buttons = [{ disabled: true }];
  const form = Object.assign(new EventTarget(), {
    submitted: 0,
    requested: 0,
    ownerDocument: { createElement: (): Field => ({ value: "" }), defaultView: view },
    querySelectorAll: () => buttons,
    append(el: Field) {
      field = el;
    },
    querySelector: (sel: string) => (sel === ".cf-turnstile" ? box : field),
    requestSubmit(submitter?: unknown) {
      if (++form.requested > 20) return;
      const e = submit(submitter);
      form.dispatchEvent(e);
      if (!e.defaultPrevented) form.submitted++;
    },
  });
  return {
    form,
    box,
    view,
    buttons,
    field: () => field,
    setField: (v: string) => {
      if (field) field.value = v;
    },
  };
}

/** A load() that counts calls and hands back a widget whose check passes when `pass()` is called. */
function fakeLoad(mode: "ok" | "fail" | "throw" = "ok") {
  const calls = { load: 0, render: [] as unknown[], reset: [] as unknown[] };
  let pass = () => {};
  let error = () => {};
  const turnstile: Turnstile = {
    render(el, o) {
      if (mode === "throw") throw new Error("turnstile is not defined");
      calls.render.push([el, o.sitekey]);
      pass = () => o.callback("tok");
      error = () => o["error-callback"]?.("110200");
    },
    reset(el) {
      calls.reset.push(el);
    },
  };
  const load = () => {
    calls.load++;
    return mode === "fail" ? Promise.reject(new Error("blocked")) : Promise.resolve(turnstile);
  };
  return { calls, load, pass: () => pass(), error: () => error() };
}

const tick = () => new Promise((r) => setTimeout(r, 0));
/** A submit event, from a button when given one, the way a tap sends it. */
function submit(submitter?: unknown) {
  return Object.assign(new Event("submit", { cancelable: true }), { submitter });
}

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
  pass();
  await tick();
  expect(field()?.value).toBe("tok");
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
  const { load } = fakeLoad("fail");
  lazyCheck(form as unknown as HTMLFormElement, load);
  form.dispatchEvent(submit());
  await tick();
  expect(form.submitted).toBe(1);
  const again = submit();
  form.dispatchEvent(again);
  expect(again.defaultPrevented).toBe(false);
});

test("a token that expired after the first check waits for the next one, without looping", async () => {
  const { form, setField } = fakeForm();
  const { load, pass } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  form.dispatchEvent(new Event("focusin"));
  await tick();
  setField("tok");
  pass();
  await tick();
  setField("");
  form.dispatchEvent(submit());
  for (let i = 0; i < 5; i++) await tick();
  expect(form.requested).toBe(0);
  expect(form.submitted).toBe(0);
  setField("tok");
  pass();
  await tick();
  expect(form.submitted).toBe(1);
});

test("if the widget errors, the held form goes, and the server asks for the check", async () => {
  const { form } = fakeForm();
  const { load, error } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  form.dispatchEvent(submit());
  await tick();
  error();
  await tick();
  expect(form.submitted).toBe(1);
});

test("two submits before the check send the form once", async () => {
  const { form, setField } = fakeForm();
  const { load, pass } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  const first = submit();
  const second = submit();
  form.dispatchEvent(first);
  form.dispatchEvent(second);
  expect(second.defaultPrevented).toBe(true);
  await tick();
  setField("tok");
  pass();
  await tick();
  expect(form.requested).toBe(1);
  expect(form.submitted).toBe(1);
});

test("if rendering throws, the held form goes, and the server asks for the check", async () => {
  const { form } = fakeForm();
  const { load } = fakeLoad("throw");
  lazyCheck(form as unknown as HTMLFormElement, load);
  form.dispatchEvent(submit());
  await tick();
  expect(form.submitted).toBe(1);
});

test("the token gets a field even if Turnstile didn't add one", async () => {
  const { form, field } = fakeForm(null);
  const { load, pass } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  form.dispatchEvent(submit());
  await tick();
  pass();
  await tick();
  expect(field()).toEqual({ value: "tok", type: "hidden", name: "cf-turnstile-response" });
  expect(form.submitted).toBe(1);
});

test("the button that sent the form is disabled, so a double tap sends one order", async () => {
  const { form } = fakeForm("tok");
  const { load } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  const button = { disabled: false };
  const e = submit(button);
  form.dispatchEvent(e);
  expect(e.defaultPrevented).toBe(false);
  expect(button.disabled).toBe(true);
});

test("a held submit disables its button only when the form actually goes", async () => {
  const { form } = fakeForm();
  const { load, pass } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  const button = { disabled: false };
  form.dispatchEvent(submit(button));
  await tick();
  expect(button.disabled).toBe(false);
  pass();
  await tick();
  expect(form.submitted).toBe(1);
  expect(button.disabled).toBe(true);
});

/** The browser restoring the page from the back/forward cache, or not. */
const pageshow = (persisted: boolean) => Object.assign(new Event("pageshow"), { persisted });

test("back from the next page, the spent token is dropped and the check runs again", async () => {
  const { form, box, view, buttons, field, setField } = fakeForm();
  const { calls, load, pass } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  form.dispatchEvent(new Event("focusin"));
  await tick();
  pass();
  setField("tok");
  view.dispatchEvent(pageshow(true));
  expect(buttons[0]?.disabled).toBe(false);
  expect(field()?.value).toBe("");
  expect(calls.reset).toEqual([box]);
  // So the next submit waits for a fresh token instead of resending the old one.
  const e = submit();
  form.dispatchEvent(e);
  expect(e.defaultPrevented).toBe(true);
});

test("a first visit, or a restore before Turnstile loaded, resets nothing", async () => {
  const { form, view, field } = fakeForm("tok");
  const { calls, load } = fakeLoad();
  lazyCheck(form as unknown as HTMLFormElement, load);
  view.dispatchEvent(pageshow(false));
  expect(field()?.value).toBe("tok");
  view.dispatchEvent(pageshow(true));
  expect(field()?.value).toBe("");
  expect(calls.reset).toEqual([]);
});
