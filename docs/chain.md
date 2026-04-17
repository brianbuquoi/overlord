# Chain mode

Chain mode is Overlord's lightweight authoring layer for linear prompt
workflows. Authors write a short YAML describing a sequence of model
calls; the chain compiler lowers that YAML into the same
`config.Config` the broker runtime already accepts, and the compiled
pipeline runs on the existing engine — retries, sanitizer envelope,
contracts, metrics, and tracing included.

Chain mode is the recommended first stop when you want to:

- Wire Claude into a multi-step prompt without thinking about schema
  versions, stage routing, or retry budgets up front.
- Stand up a two- or three-call workflow locally with zero credentials
  via the `mock` provider.
- Prototype something that *might* graduate into a full pipeline
  later, without throwing the prototype away when it does.

Chain mode is **not** a separate engine, a separate format, or a
separate runtime. It is a front door.

## When to choose chain vs full pipeline

| You want to…                                                  | Use chain | Use pipeline |
|---------------------------------------------------------------|:---------:|:------------:|
| Run a short `draft → review` or `summarize → critique` flow   |     ✓     |              |
| Demo a multi-model idea with no credentials                   |     ✓     |              |
| Author one YAML file for a linear workflow                    |     ✓     |              |
| Use fan-out across multiple reviewers in parallel             |           |      ✓       |
| Route tasks conditionally based on agent output               |           |      ✓       |
| Declare explicit schemas, retry budgets, or dead-letter paths |           |      ✓       |
| Run a long-lived server with the HTTP API and dashboard       |           |      ✓       |
| Split infra config from pipeline config                       |           |      ✓       |

If you are not sure, start with chain. Everything you author in chain
mode can be emitted as pipeline YAML via `overlord chain export` when
the workflow is ready to graduate.

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
  Resolved at runtime. Intermediate steps always expose their output
  as text (the `"text"` field of the inter-stage payload); use JSON
  output only on the last step.

Forward references (`{{steps.X.output}}` from a step that runs before
`X`) are a compile-time error.

### Supported providers

Any provider the runtime already understands: `anthropic`, `openai`,
`openai-responses`, `google`, `ollama`, `mock`, `copilot`. Write the
model string as `<provider>/<model>` (e.g. `anthropic/claude-sonnet-4-5`,
`openai/gpt-4o`). A bare provider name (e.g. `anthropic` with no
model) is **rejected** for every real provider — chain validation
surfaces a clear error telling you to pick a concrete model. Bare
`mock` is the one exception: mock steps never consult a model string,
so `model: mock` is allowed. Mock steps also require a `fixture:`
path, which must be a JSON file relative to the chain YAML's
directory.

### Input and output

- `input.type: text` wraps the raw string as `{"text": "..."}` on the
  wire so every built-in adapter sees a uniform object. `{{input}}`
  in a step prompt renders as the raw string.
- `input.type: json` accepts a JSON **object** verbatim — the object
  is forwarded to the first stage unchanged. `{{input}}` in a step
  prompt renders as the **full original JSON object string** (e.g.
  `{"query":"foo","limit":5}`). No field is silently extracted — the
  whole object is what your prompt sees.
- `output.type: text` (default) unwraps the final step's `.text` field
  when printing.
- `output.type: json` treats the final step's payload as structured
  JSON; the LLM is expected to produce a JSON object. By default any
  object passes the schema check; see "Validating JSON output" below
  to add required fields or types inline.

### Selecting the final output

`chain.output.from` is optional and, when set, must reference the
chain's **last** step in the `steps.<id>.output` form. Picking an
intermediate step is **not** supported in chain mode v1 — the
compiler would have to either surface a non-terminal task's payload
or truncate the pipeline, both of which break the "chains are normal
pipelines" model. If you need intermediate-step output, graduate via
`overlord chain export` and hand-edit the exported pipeline: every
stage can route to `done` explicitly. Chain validation rejects
non-last-step `output.from` references with a pointer at this
limitation.

### Validating JSON output

For `output.type: json`, you can optionally declare a lightweight
inline JSONSchema the final stage's payload is validated against.
This is a strict subset of what pipeline-mode's `schema_registry`
gives you — the point is to catch missing fields before you graduate,
not to recreate the full schema machinery.

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

Rules:

- `output.schema` is only valid when `output.type: json`. Setting it
  with `type: text` (or no type) is a validation error.
- The schema is any valid JSONSchema fragment — whatever you write is
  serialized to JSON and compiled by the same JSONSchema compiler
  pipeline mode uses. Invalid schemas are rejected at load time —
  `chain run`, `chain inspect`, and `chain export` all surface the
  error before the broker starts, never mid-run.
- The schema is synthesized under the reserved name `chain_json@v1`.
  `chain export` writes it to `schemas/chain_json_v1.json`, so
  graduation preserves the validation rules.
- v1 is intentionally narrow: no `$ref`, no cross-file references,
  no version bumping. For those, graduate.

## CLI

```bash
# Scaffold a starter chain project (uses the mock provider).
overlord chain init write-review

# Run a chain with a text input.
overlord chain run --chain ./chain.yaml --input "some text"

# Or read the input from a file.
overlord chain run --chain ./chain.yaml --input-file ./prompt.txt

# Read the input from stdin (the single-dash sentinel reads until EOF,
# so pipes and heredocs work naturally).
echo "summarize this" | overlord chain run --chain ./chain.yaml --input-file -

# Show the compiled interpretation (pipeline, stages, agents, schemas).
overlord chain inspect --chain ./chain.yaml

# Emit the equivalent full pipeline YAML + schemas for graduation.
overlord chain export --chain ./chain.yaml --out ./pipeline/
```

`chain inspect` prints:

- The synthesized pipeline name, stage list, and per-stage agent
  binding (provider + model).
- The input/output schemas and routing.
- The *resolved* system-prompt template for each step, with `{{vars.*}}`
  substituted and `{{input}}` / `{{steps.X.output}}` preserved.

## Graduating to full pipeline mode

`overlord chain export --chain ./chain.yaml --out ./pipeline/` writes:

```
pipeline/
  overlord.yaml          # full-pipeline-mode config
  schemas/               # synthesized schemas (chain_text_v1.json, …)
  fixtures/              # any mock fixtures referenced by the chain
```

The exported directory is a **normal pipeline-mode project**. Run it
with:

```bash
overlord validate --config pipeline/overlord.yaml
overlord exec --config pipeline/overlord.yaml --id <chain-id> \
  --payload '{"text":"..."}'
overlord run --config pipeline/overlord.yaml
```

From there, hand-edit `overlord.yaml` to add whatever full pipeline
mode offers — fan-out, conditional routing, retry budgets, dead-letter
policies, authentication, tracing, dashboard config. Nothing about
graduation is reversible in the runtime sense: the chain YAML is your
source, the exported pipeline is the artifact.

## Internals (for the curious)

Chain mode preserves the full strict runtime. What the compile layer
does:

- Parses the chain YAML and validates step IDs, references, variables,
  and provider names.
- Synthesizes JSON schemas on the fly and registers them via
  `contract.NewRegistryFromRaw`:
  - `chain_text@v1` — the inter-stage text wire format
    (`{"text": "..."}`) used between every step.
  - `chain_json@v1` — the final-stage contract when
    `output.type: json`. Defaults to an open object, or the
    compiled form of `output.schema` when the author declared one.
  - `chain_input_json@v1` — an open-object contract for the first
    stage's input when `input.type: json`. Distinct from
    `chain_json@v1` so an author tightening the output schema does
    not inadvertently reject the initial JSON payload.
- Emits a normal `config.Config` with one pipeline, one stage per step,
  and one agent per step. The agent's `system_prompt` holds the step's
  prompt with `{{vars.*}}` resolved — runtime placeholders are
  preserved intact.
- Wraps each built-in adapter in a `chain.NewStepAdapter`. The wrapper
  substitutes runtime placeholders (`{{input}}`, `{{steps.X.output}}`)
  against values carried in `task.Metadata["chain"]`, records its
  own output into the same metadata map for later steps to reference,
  and delegates the actual LLM call to the wrapped adapter.

The broker never sees chain mode. It runs a normal pipeline with a
normal agent set. The sanitizer envelope still wraps prior-stage
output on every non-first stage; the contract validator still rejects
non-conforming payloads; retries, dead-letter, and tracing all apply.
