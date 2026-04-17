# Chain mode (legacy authoring surface)

Chain mode is the prior authoring layer, kept in the binary for
projects already running on `overlord chain run` / `overlord chain
export`. New projects should start with the workflow format
(`workflow:` in `overlord.yaml`) — see the README and
[docs/init.md](init.md) — which shares the same compile path under
the hood.

This document describes chain YAML and the `overlord chain …`
subcommands. It is not the beginner-facing product story.

## Why chain mode still exists

The workflow surface (default product story) compiles through the
chain layer via `internal/workflow/compile.go` →
`chain.CompileWithBase`. Keeping the chain commands live means:

- projects authored against the chain format continue to work with
  no forced migration;
- the compile path has one source of truth (`internal/chain`), and
  the workflow surface is a thin translator on top;
- the graduation story (`chain export` → strict YAML) still works
  for chain-authored projects.

If you do not have a chain-authored project already, read the
README and use `overlord run` / `overlord serve` / `overlord init`
against a workflow file instead.

## The shape

```yaml
version: "1"

chain:
  id: write-and-review

  input:
    type: text

  vars:
    topic: "Write a short design note for a CLI feature."

  steps:
    - id: draft
      model: anthropic/claude-sonnet-4-5
      prompt: |
        You are writing a first draft.

        Task:
        {{vars.topic}}

        Source material:
        {{input}}

    - id: review
      model: openai/gpt-4o
      prompt: |
        Review the draft below.

        Focus on correctness, missing edge cases, and implementation
        risks.

        Original task: {{vars.topic}}

        Draft:
        {{steps.draft.output}}

  output:
    from: steps.review.output
    type: text
```

### Placeholders

Three placeholders are supported in a step's `prompt`:

- `{{input}}` — the chain's initial input.
  - For `input.type: text` this is the raw string the caller passed
    to `--input` / `--input-file`.
  - For `input.type: json` this is the full JSON object string — the
    whole payload, not a single field of it.
- `{{vars.<name>}}` — a variable declared in the chain's `vars` map.
  Resolved at compile time.
- `{{steps.<id>.output}}` — the text output of an earlier step.

Chain mode does **not** support the `{{prev}}` shorthand; that is a
workflow-mode ergonomic. Translate `{{prev}}` → `{{steps.<id>.output}}`
when porting a workflow to chain mode.

### Supported providers

Any provider the runtime understands: `anthropic`, `openai`,
`openai-responses`, `google`, `ollama`, `mock`, `copilot`. Write the
model string as `<provider>/<model>`. A bare provider name (e.g.
`anthropic` with no model) is **rejected** for every real provider;
bare `mock` is allowed.

### Input and output

- `input.type: text` wraps the raw string as `{"text": "..."}` on
  the wire. `{{input}}` in a step prompt renders as the raw string.
- `input.type: json` accepts a JSON **object** verbatim. `{{input}}`
  renders as the full JSON object string.
- `output.type: text` (default) unwraps the final step's `.text`
  field when printing.
- `output.type: json` treats the final step's payload as structured
  JSON. Optional inline schema validates the final payload.

### Selecting the final output

`chain.output.from` is optional and, when set, must reference the
chain's last step in the `steps.<id>.output` form. Picking an
intermediate step is not supported — graduate via `overlord chain
export` and hand-edit the exported strict config.

### Validating JSON output

```yaml
output:
  type: json
  schema:
    type: object
    required: [summary, findings, verdict]
    properties:
      summary:  { type: string }
      findings: { type: array, items: { type: object } }
      verdict:  { type: string, enum: [approve, reject, revise] }
```

## CLI

```bash
# Scaffold a chain project (uses the mock provider).
overlord chain init write-review

# Run a chain with a text input.
overlord chain run --chain ./chain.yaml --input "some text"
overlord chain run --chain ./chain.yaml --input-file ./prompt.txt
echo "summarize this" | overlord chain run --chain ./chain.yaml --input-file -

# Show the compiled interpretation (pipeline, stages, agents, schemas).
overlord chain inspect --chain ./chain.yaml

# Emit the equivalent strict pipeline YAML + schemas for graduation.
overlord chain export --chain ./chain.yaml --out ./pipeline/
```

`chain inspect` prints the synthesized pipeline name, each step's
agent binding (provider + model), input/output schemas, and the
resolved system-prompt template.

## Graduating to strict mode

`overlord chain export --chain ./chain.yaml --out ./pipeline/` writes
the strict-mode equivalent of a chain. The exported directory is a
normal strict-mode project runnable under `overlord exec` /
`overlord serve`.

Workflow users have a parallel command: `overlord export --advanced
--out ./advanced` is the workflow → strict graduation path. Both
commands emit the same shape and share the same
`chain.Export` / `workflow.Export` implementation.

## Internals

Chain mode preserves the full strict runtime:

- `chain.Load` + `chain.Validate` parse and check the YAML.
- `chain.CompileWithBase` lowers the chain into a `config.Config`
  with synthesized schemas:
  - `chain_text@v1` — the inter-stage text wire format
    (`{"text": "..."}`).
  - `chain_json@v1` — the final-stage contract when
    `output.type: json`.
  - `chain_input_json@v1` — the first-stage input contract when
    `input.type: json`.
- `chain.NewStepAdapter` wraps each built-in adapter with runtime
  placeholder substitution and per-step output metadata.

The strict-pipeline broker runs the compiled config verbatim. The
sanitizer envelope still wraps prior-stage output on every non-first
stage; the contract validator still rejects non-conforming payloads;
retries, dead-letter, and tracing all apply.

Workflow mode (`internal/workflow/compile.go`) rides the same
machinery: it auto-generates step IDs, rewrites `{{prev}}`, translates
the workflow into a `chain.Chain`, and delegates to
`chain.CompileWithBase`. Every feature added to the chain compile
layer is automatically available to workflow authors.
