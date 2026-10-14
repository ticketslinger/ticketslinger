import type { components } from "@ticketslinger/api-client";
import { useEffect, useRef, useState } from "react";
import { api, must } from "../api";
import { ErrorText, Field, useAction, useLoad } from "../ui";
import type { Event } from "./EventForm";
import { canEdit, type Org } from "./Organizer";
import {
  addOption,
  duplicateAt,
  moveOption,
  removeOption,
  renameOption,
  swapPositions,
} from "./optionList";
import { FormDialog, type Problem, Remove, toggle, useProblem } from "./Tickets";

type Question = components["schemas"]["Question"];
type TicketType = components["schemas"]["TicketType"];

/** Which form field an API error code is about; "" when it's about the form as a whole. */
export function questionFieldFor(code: string, message: string): string {
  switch (code) {
    case "bad_label":
      return "label";
    case "bad_kind":
      return "kind";
    case "bad_scope":
      return "scope";
    case "bad_options":
    case "options_required_with_kind":
      return "options";
    case "applies_to_conflict":
    case "applies_to_required":
    case "unknown_ticket_type":
      return "types";
    case "missing_field":
      return message.includes("label") ? "label" : message.includes("scope") ? "scope" : "";
    default:
      return "";
  }
}

export type QuestionDraft = {
  label: string;
  kind: Question["kind"];
  options: string[];
  required: boolean;
  scope: Question["scope"];
  all: boolean;
  types: string[];
};

export const emptyQuestion: QuestionDraft = {
  label: "",
  kind: "text",
  options: ["", ""],
  required: false,
  scope: "order",
  all: true,
  types: [],
};

const draftOf = (q: Question): QuestionDraft => ({
  label: q.label,
  kind: q.kind,
  options: q.kind === "select" ? q.options : ["", ""],
  required: q.required,
  scope: q.scope,
  all: q.applies_to_all,
  types: q.applies_to,
});

/** Checks a question form and turns it into the API's fields, or says which field is wrong. */
export function questionBody(
  d: QuestionDraft,
): { body: components["schemas"]["QuestionFields"] } | Problem {
  const label = d.label.trim();
  const options = d.kind === "select" ? d.options.map((o) => o.trim()) : [];
  if (!label || [...label].length > 200)
    return { field: "label", message: "Give the question a label of up to 200 characters." };
  if (d.kind === "select") {
    if (options.length < 2 || options.length > 30)
      return { field: "options", message: "A select needs 2 to 30 options." };
    const bad = options.findIndex((o) => !o || [...o].length > 120);
    if (bad >= 0)
      return {
        field: `option-${bad}`,
        message: "Each option needs 1 to 120 characters. Fill this one in or remove it.",
      };
    const dup = duplicateAt(options);
    if (dup >= 0)
      return {
        field: `option-${dup}`,
        message: `“${options[dup]}” is listed twice. Options have to be different, ignoring capitals.`,
      };
  }
  if (!d.all && d.types.length === 0)
    return { field: "types", message: "Pick at least one ticket, or choose all tickets." };
  return {
    body: {
      label,
      kind: d.kind,
      options,
      required: d.required,
      scope: d.scope,
      applies_to_all: d.all,
      applies_to: d.all ? [] : d.types,
    },
  };
}

const arrow = { "-1": "up", "1": "down" } as const;

function OptionsEditor({
  options,
  onChange,
  describedBy,
}: {
  options: string[];
  onChange: (o: string[]) => void;
  describedBy?: string;
}) {
  // The control to focus once the list has re-rendered; moving a row remounts nothing, but focus must follow it.
  const [focus, setFocus] = useState<string>();
  // biome-ignore lint/correctness/useExhaustiveDependencies: options changing is the trigger
  useEffect(() => {
    if (!focus) return;
    const el = document.getElementById(focus) as HTMLButtonElement | null;
    el?.focus();
    setFocus(undefined);
  }, [options, focus]);

  const move = (i: number, dir: -1 | 1) => {
    const to = i + dir;
    onChange(moveOption(options, i, dir));
    // At the end of the list that arrow is disabled, so land on the other one.
    const edge = to === 0 || to === options.length - 1;
    setFocus(`qn-option-${to}-${arrow[edge ? (-dir as -1 | 1) : dir]}`);
  };

  return (
    <fieldset className="choices" id="qn-options" tabIndex={-1} aria-describedby={describedBy}>
      <legend>Options</legend>
      <p className="hint">Buyers pick one. Add 2 to 30, each different, ignoring capitals.</p>
      {options.map((o, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: rows have no identity of their own
        <div className="option-row" key={i}>
          <input
            id={`qn-option-${i}`}
            aria-label={`Option ${i + 1}`}
            maxLength={120}
            value={o}
            onChange={(e) => onChange(renameOption(options, i, e.target.value))}
          />
          <button
            type="button"
            id={`qn-option-${i}-up`}
            className="inline secondary"
            aria-label={`Move option ${i + 1} up`}
            disabled={i === 0}
            onClick={() => move(i, -1)}
          >
            Up
          </button>
          <button
            type="button"
            id={`qn-option-${i}-down`}
            className="inline secondary"
            aria-label={`Move option ${i + 1} down`}
            disabled={i === options.length - 1}
            onClick={() => move(i, 1)}
          >
            Down
          </button>
          <button
            type="button"
            className="inline secondary"
            aria-label={`Remove option ${i + 1}`}
            onClick={() => {
              onChange(removeOption(options, i));
              setFocus(
                options.length > 1
                  ? `qn-option-${Math.min(i, options.length - 2)}`
                  : "qn-add-option",
              );
            }}
          >
            Remove
          </button>
        </div>
      ))}
      <button
        type="button"
        id="qn-add-option"
        className="inline secondary"
        disabled={options.length >= 30}
        onClick={() => {
          onChange(addOption(options));
          setFocus(`qn-option-${options.length}`);
        }}
      >
        Add an option
      </button>
    </fieldset>
  );
}

function QuestionDialog({
  org,
  event,
  question,
  tickets,
  onClose,
  onSaved,
}: {
  org: Org;
  event: Event;
  question: Question | null;
  tickets: TicketType[];
  onClose: () => void;
  onSaved: () => void;
}) {
  const start = question ? draftOf(question) : emptyQuestion;
  const [d, setD] = useState(start);
  const [local, setLocal] = useState<Problem | null>(null);
  const { busy, error, code, clear, run } = useAction();
  const p = useProblem("qn", local, error, code, questionFieldFor);
  const set = (patch: Partial<QuestionDraft>) => {
    setLocal(null);
    clear();
    setD((old) => ({ ...old, ...patch }));
  };

  function save() {
    const r = questionBody(d);
    if ("field" in r) return setLocal(r);
    run(async () => {
      const path = { organizer: org.slug, event: event.id };
      if (question) {
        must(
          await api.PATCH("/organizers/{organizer}/events/{event}/questions/{question}", {
            params: { path: { ...path, question: question.id } },
            body: r.body,
          }),
        );
      } else {
        must(
          await api.POST("/organizers/{organizer}/events/{event}/questions", {
            params: { path },
            body: {
              ...r.body,
              label: r.body.label ?? "",
              kind: r.body.kind ?? "text",
              scope: r.body.scope ?? "order",
            },
          }),
        );
      }
      onSaved();
    });
  }

  return (
    <FormDialog
      title={question ? "Edit question" : "Add a question"}
      dirty={JSON.stringify(d) !== JSON.stringify(start)}
      onClose={onClose}
      onSubmit={save}
      busy={busy}
      submitLabel={question ? "Save changes" : "Add question"}
    >
      <fieldset className="plain">
        <Field
          id="qn-label"
          label="Question"
          hint="What buyers see, like “Any dietary needs?”"
          required
          maxLength={200}
          aria-invalid={p.bad("label")}
          value={d.label}
          onChange={(e) => set({ label: e.target.value })}
        />
        <div className="field">
          <label htmlFor="qn-kind">Answer with</label>
          <select
            id="qn-kind"
            value={d.kind}
            onChange={(e) => set({ kind: e.target.value as QuestionDraft["kind"] })}
          >
            <option value="text">A text box</option>
            <option value="select">A choice from a list</option>
            <option value="checkbox">A checkbox</option>
          </select>
        </div>
        {d.kind === "select" && (
          <OptionsEditor
            options={d.options}
            onChange={(options) => set({ options })}
            describedBy={p.describe("options") ?? p.describe(`option-${duplicateAt(d.options)}`)}
          />
        )}
        <label className="check">
          <input
            type="checkbox"
            checked={d.required}
            onChange={(e) => set({ required: e.target.checked })}
          />
          <span>
            Buyers have to answer
            <span className="hint">They can't check out until they do.</span>
          </span>
        </label>
        <fieldset className="choices" id="qn-scope" tabIndex={-1}>
          <legend>Ask</legend>
          <label className="check">
            <input
              type="radio"
              name="qn-scope"
              checked={d.scope === "order"}
              onChange={() => set({ scope: "order" })}
            />
            <span>Once per order</span>
          </label>
          <label className="check">
            <input
              type="radio"
              name="qn-scope"
              checked={d.scope === "ticket"}
              onChange={() => set({ scope: "ticket" })}
            />
            <span>For every ticket</span>
          </label>
        </fieldset>
        <fieldset
          className="choices"
          id="qn-types"
          tabIndex={-1}
          aria-describedby={p.describe("types")}
        >
          <legend>Which tickets</legend>
          <label className="check">
            <input
              type="radio"
              name="qn-all"
              checked={d.all}
              onChange={() => set({ all: true, types: [] })}
            />
            <span>Every ticket</span>
          </label>
          <label className="check">
            <input
              type="radio"
              name="qn-all"
              checked={!d.all}
              onChange={() => set({ all: false })}
            />
            <span>Only the tickets I pick</span>
          </label>
          {!d.all && tickets.length === 0 && (
            <p className="hint">Add a ticket first, then come back to pick it.</p>
          )}
          {!d.all &&
            tickets.map((t) => (
              <label className="check" key={t.id}>
                <input
                  type="checkbox"
                  checked={d.types.includes(t.id)}
                  onChange={() => set({ types: toggle(d.types, t.id) })}
                />
                <span>{t.name}</span>
              </label>
            ))}
        </fieldset>
      </fieldset>
      <ErrorText id={`${p.prefix}-error`}>{p.message}</ErrorText>
    </FormDialog>
  );
}

const kindText = { text: "Text box", select: "Choice", checkbox: "Checkbox" } as const;

/** The Questions tab: what you ask buyers at checkout, in the order they see it. */
export function Questions({ org, event }: { org: Org; event: Event }) {
  const path = { organizer: org.slug, event: event.id };
  const qs = useLoad(async () =>
    must(await api.GET("/organizers/{organizer}/events/{event}/questions", { params: { path } })),
  );
  const types = useLoad(async () =>
    must(
      await api.GET("/organizers/{organizer}/events/{event}/ticket-types", { params: { path } }),
    ),
  );
  // undefined: closed; null: adding; otherwise editing that one.
  const [question, setQuestion] = useState<Question | null | undefined>();
  const move = useAction();
  const focusAfter = useRef<string | undefined>(undefined);
  const editor = canEdit(org);
  const list = qs.data ?? [];
  const tickets = types.data ?? [];
  const names = new Map(tickets.map((t) => [t.id, t.name]));

  // After a move, focus follows the question; at an end that arrow is disabled, so use the other.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the reloaded list is the trigger
  useEffect(() => {
    if (!focusAfter.current) return;
    document.getElementById(focusAfter.current)?.focus();
    focusAfter.current = undefined;
  }, [qs.data]);

  const step = (i: number, dir: -1 | 1) =>
    move.run(async () => {
      const swap = swapPositions(list, i, dir);
      if (!swap) return;
      const id = list[i]?.id;
      const edge = i + dir === 0 || i + dir === list.length - 1;
      focusAfter.current = `qs-${id}-${arrow[edge ? (-dir as -1 | 1) : dir]}`;
      const patch = (s: { id: string; position: number }) =>
        api.PATCH("/organizers/{organizer}/events/{event}/questions/{question}", {
          params: { path: { ...path, question: s.id } },
          body: { position: s.position },
        });
      try {
        must(await patch(swap[0] as (typeof swap)[0]));
        must(await patch(swap[1] as (typeof swap)[0]));
      } catch (e) {
        // Put the first one back if the second failed, so two questions don't share a place.
        const a = list[i];
        if (a) await patch({ id: a.id, position: a.position }).catch(() => {});
        focusAfter.current = undefined;
        await qs.reload();
        throw e;
      }
      await qs.reload();
    });

  return (
    <>
      <section aria-labelledby="questions-h">
        <div className="heading-row">
          <h2 id="questions-h">Questions</h2>
          {editor && (
            <button type="button" className="inline" onClick={() => setQuestion(null)}>
              Add a question
            </button>
          )}
        </div>
        <p className="muted">
          Ask buyers for what you need at checkout, like a dietary need or a company name. They see
          the questions in this order.
        </p>
        <ErrorText>{qs.error?.message ?? types.error?.message ?? move.error}</ErrorText>
        {!qs.data && !qs.error && <p className="muted">Loading…</p>}
        {qs.data?.length === 0 && (
          <p>
            No questions yet.{" "}
            {editor
              ? "Add one if you need to know something about your buyers."
              : "They'll show up here once someone on your team adds them."}
          </p>
        )}
        {list.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th scope="col">Question</th>
                  <th scope="col">Answer</th>
                  <th scope="col">Asked</th>
                  <th scope="col">Tickets</th>
                  {editor && <th scope="col">Actions</th>}
                </tr>
              </thead>
              <tbody>
                {list.map((q, i) => (
                  <tr key={q.id}>
                    <td>
                      {q.label} {q.required && <span className="pill">Required</span>}
                    </td>
                    <td>
                      {kindText[q.kind]}
                      {q.kind === "select" && <span className="hint">{q.options.join(", ")}</span>}
                    </td>
                    <td>{q.scope === "order" ? "Once per order" : "For every ticket"}</td>
                    <td>
                      {q.applies_to_all
                        ? "Every ticket"
                        : q.applies_to
                            .map((id) => names.get(id))
                            .filter(Boolean)
                            .join(", ") || <span className="muted">No tickets left</span>}
                    </td>
                    {editor && (
                      <td className="actions-cell">
                        <div className="row">
                          <button
                            type="button"
                            id={`qs-${q.id}-up`}
                            className="inline secondary"
                            aria-label={`Move ${q.label} up`}
                            disabled={i === 0 || move.busy}
                            onClick={() => step(i, -1)}
                          >
                            Up
                          </button>
                          <button
                            type="button"
                            id={`qs-${q.id}-down`}
                            className="inline secondary"
                            aria-label={`Move ${q.label} down`}
                            disabled={i === list.length - 1 || move.busy}
                            onClick={() => step(i, 1)}
                          >
                            Down
                          </button>
                          <button
                            type="button"
                            className="inline secondary"
                            aria-label={`Edit ${q.label}`}
                            onClick={() => setQuestion(q)}
                          >
                            Edit
                          </button>
                          <Remove
                            label={q.label}
                            noun="question"
                            run={async () => {
                              must(
                                await api.DELETE(
                                  "/organizers/{organizer}/events/{event}/questions/{question}",
                                  { params: { path: { ...path, question: q.id } } },
                                ),
                              );
                              await qs.reload();
                            }}
                          />
                        </div>
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {question !== undefined && (
        <QuestionDialog
          org={org}
          event={event}
          question={question}
          tickets={tickets}
          onClose={() => setQuestion(undefined)}
          onSaved={() => {
            setQuestion(undefined);
            qs.reload();
          }}
        />
      )}
    </>
  );
}
