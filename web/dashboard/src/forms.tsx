import type { components } from "@ticketslinger/api-client";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { askToLeave, ErrorText, focusOnMount, useAction, useUnsavedChanges } from "./ui";

type TicketType = components["schemas"]["TicketType"];

export const LOCAL = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?$/;
export const MAX_CENTS = 10_000_000;

/** A field that failed, and what to tell the organizer about it. */
export type Problem = { field: string; message: string };

export const toggle = (list: string[], id: string) =>
  list.includes(id) ? list.filter((x) => x !== id) : [...list, id];

/** A native modal: closes on Escape or Cancel, and asks before throwing away typing. */
export function FormDialog({
  title,
  dirty,
  onClose,
  onSubmit,
  busy,
  submitLabel,
  children,
}: {
  title: string;
  dirty: boolean;
  onClose: () => void;
  onSubmit: () => void;
  busy: boolean;
  submitLabel: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const leave = useUnsavedChanges(dirty);
  useEffect(() => {
    if (ref.current && !ref.current.open) ref.current.showModal();
  }, []);
  return (
    <dialog
      ref={ref}
      className="form"
      aria-labelledby="form-dialog-title"
      onClose={onClose}
      onCancel={(e) => {
        if (!dirty) return;
        e.preventDefault();
        askToLeave(() => ref.current?.close());
      }}
    >
      <h2 id="form-dialog-title">{title}</h2>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit();
        }}
      >
        {children}
        <div className="row">
          <button type="submit" className="inline" disabled={busy}>
            {submitLabel}
          </button>
          <button
            type="button"
            className="inline secondary"
            onClick={() => askToLeave(() => ref.current?.close())}
          >
            Cancel
          </button>
        </div>
      </form>
      {leave}
    </dialog>
  );
}

/** Marks the field a problem is about and moves focus to it. */
export function useProblem(
  prefix: string,
  local: Problem | null,
  error: string,
  code: string,
  fieldOf: (code: string, message: string) => string,
) {
  const field = local?.field ?? (code ? fieldOf(code, error) : "");
  const message = local?.message ?? error;
  // biome-ignore lint/correctness/useExhaustiveDependencies: message re-runs it, so a repeat mistake pulls focus back
  useEffect(() => {
    if (field) document.getElementById(`${prefix}-${field}`)?.focus();
  }, [prefix, field, message]);
  return {
    message,
    prefix,
    bad: (f: string) => (field === f ? true : undefined),
    // For a group of controls, which can't be aria-invalid: point at the message instead.
    describe: (f: string) => (field === f && message ? `${prefix}-error` : undefined),
  };
}

/** Asks "sure?" in place of the Delete button, then deletes. */
export function Remove({
  label,
  noun,
  run,
}: {
  label: string;
  noun: string;
  run: () => Promise<void>;
}) {
  const { busy, error, run: go } = useAction();
  const [asking, setAsking] = useState<boolean | null>(null);
  return (
    <>
      {asking ? (
        <span className="row">
          <button
            type="button"
            className="inline"
            ref={focusOnMount}
            disabled={busy}
            onClick={() => go(run)}
          >
            Delete {noun}
          </button>
          <button type="button" className="inline secondary" onClick={() => setAsking(false)}>
            Keep it
          </button>
        </span>
      ) : (
        <button
          type="button"
          className="inline secondary"
          ref={asking === false ? focusOnMount : undefined}
          aria-label={`Delete ${label}`}
          onClick={() => setAsking(true)}
        >
          Delete
        </button>
      )}
      <ErrorText>{error}</ErrorText>
    </>
  );
}

/** "Which tickets": every ticket, or the ones ticked. Focus lands on the group when it has a problem. */
export function TicketPicker({
  id,
  legend,
  tickets,
  all,
  types,
  onChange,
  describedBy,
  hint,
}: {
  id: string;
  legend: string;
  tickets: TicketType[];
  all: boolean;
  types: string[];
  onChange: (v: { all: boolean; types: string[] }) => void;
  describedBy?: string;
  hint?: ReactNode;
}) {
  return (
    <fieldset className="choices" id={id} tabIndex={-1} aria-describedby={describedBy}>
      <legend>{legend}</legend>
      <label className="check">
        <input
          type="radio"
          name={id}
          checked={all}
          onChange={() => onChange({ all: true, types: [] })}
        />
        <span>Every ticket</span>
      </label>
      <label className="check">
        <input
          type="radio"
          name={id}
          checked={!all}
          onChange={() => onChange({ all: false, types })}
        />
        <span>Only the tickets I pick</span>
      </label>
      {hint && <p className="hint">{hint}</p>}
      {!all && tickets.length === 0 && (
        <p className="hint">Add a ticket first, then come back to pick it.</p>
      )}
      {!all &&
        tickets.map((t) => (
          <label className="check" key={t.id}>
            <input
              type="checkbox"
              checked={types.includes(t.id)}
              onChange={() => onChange({ all: false, types: toggle(types, t.id) })}
            />
            <span>{t.name}</span>
          </label>
        ))}
    </fieldset>
  );
}
