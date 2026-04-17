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

- `{{input}}` — the chain's initial text (or JSON blob) submitted at
  run time.
- `{{vars.<name>}}` — a variable declared in the chain's `vars` map.
  Resolved at compile time.
- `{{steps.<id>.output>}}` — the text output of an earlier step.
  Resolved at runtime.

Forward references (`{{steps.X.output}}` from a step that runs before
`X`) are a compile-time error.

### Supported providers

Any provider the runtime already understands: `anthropic`, `openai`,
`openai-responses`, `google`, `ollama`, `mock`, `copilot`. Write the
model string as `<provider>/<model>` (e.g. `anthropic/claude-sonnet-4-5`,
`openai/gpt-4o`). Bare `mock` is accepted for mock-provider steps; a
`fixture:` path is required and must be a JSON file relative to the
chain YAML's directory.

### Input and output

- `input.type: text` wraps the raw string as `{"text": "..."}` on the
  wire so every built-in adapter sees a uniform object.
- `input.type: json` accepts a JSON **object** verbatim.
- `output.type: text` (default) unwraps the final step's `.text` field
  when printing.
- `output.type: json` treats the final step's payload as structured
  JSON; the LLM is expected to produce a JSON object.

## CLI

```bash
# Scaffold a starter chain project (uses the mock provider).
overlord chain init write-review

# Run a chain with a text input.
overlord chain run --chain ./chain.yaml --input "some text"

# Or read the input from a file (use "-" for stdin with --input-file).
overlord chain run --chain ./chain.yaml --input-file ./prompt.txt

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
- Synthesizes two JSON schemas on the fly (`chain_text@v1`,
  `chain_json@v1`) and registers them via `contract.NewRegistryFromRaw`.
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
