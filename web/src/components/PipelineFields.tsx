import { useState } from 'react'

import { Button } from '#/components/ui/button'
import { Label } from '#/components/ui/label'

// Shared textarea styling — there is no ui/textarea component and CEL/JS/JSON
// need a multi-line, monospace field. Mirrors ui/input's border/focus treatment
// (copied from the endpoint/portal editors that already define it locally).
const textareaClass =
  'flex min-h-[100px] w-full rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:ring-1 focus-visible:ring-ring focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50'

type FilterPreviewFn = (input: {
  expr: string
  payload: unknown
  outbound?: boolean
}) => Promise<{ match: boolean }>
type TransformPreviewFn = (input: { js: string; payload: unknown }) => Promise<{ result: unknown }>

// Pull the human message out of either error shape: the session client reshapes
// 400s into ApiError (.message = backend `error`), the portal client rejects the
// raw axios error (message in response.data.error).
function previewError(e: unknown): string {
  if (e && typeof e === 'object') {
    const anyE = e as { response?: { data?: { error?: string; message?: string } }; message?: string }
    const be = anyE.response?.data?.error ?? anyE.response?.data?.message
    if (typeof be === 'string' && be) return be
    if (typeof anyE.message === 'string' && anyE.message) return anyE.message
  }
  return String(e)
}

// Filter (CEL) + Transform (JS) editor fields with a shared sample-payload
// textarea and per-field Test buttons. State for the two fields lives in the
// parent (bound to save); the sample payload and results are local. Renders as
// a fragment of field <div>s so it drops into an existing form section.
export function PipelineFields({
  filterExpr,
  setFilterExpr,
  transformJs,
  setTransformJs,
  outbound,
  filterPreview,
  transformPreview,
}: {
  filterExpr: string
  setFilterExpr: (v: string) => void
  transformJs: string
  setTransformJs: (v: string) => void
  outbound: boolean
  filterPreview: FilterPreviewFn
  transformPreview: TransformPreviewFn
}) {
  const [sample, setSample] = useState('{}')
  const [filterResult, setFilterResult] = useState<string | null>(null)
  const [filterOk, setFilterOk] = useState(false)
  const [transformResult, setTransformResult] = useState<string | null>(null)
  const [transformOk, setTransformOk] = useState(false)
  const [testing, setTesting] = useState<'filter' | 'transform' | null>(null)

  // Parsed sample payload, or undefined when the JSON is invalid.
  function parseSample(): unknown | undefined {
    try {
      return JSON.parse(sample.trim() || '{}')
    } catch {
      return undefined
    }
  }

  async function testFilter() {
    const payload = parseSample()
    if (payload === undefined) {
      setFilterOk(false)
      setFilterResult('invalid sample JSON')
      return
    }
    setTesting('filter')
    setFilterResult(null)
    try {
      const r = await filterPreview({ expr: filterExpr, payload, outbound })
      setFilterOk(true)
      setFilterResult(r.match ? 'match: true' : 'match: false')
    } catch (e) {
      setFilterOk(false)
      setFilterResult(previewError(e))
    } finally {
      setTesting(null)
    }
  }

  async function testTransform() {
    const payload = parseSample()
    if (payload === undefined) {
      setTransformOk(false)
      setTransformResult('invalid sample JSON')
      return
    }
    setTesting('transform')
    setTransformResult(null)
    try {
      const r = await transformPreview({ js: transformJs, payload })
      setTransformOk(true)
      setTransformResult(JSON.stringify(r.result, null, 2))
    } catch (e) {
      setTransformOk(false)
      setTransformResult(previewError(e))
    } finally {
      setTesting(null)
    }
  }

  return (
    <>
      <div>
        <Label htmlFor="pipeline-filter" className="mb-2 block">
          Filter (CEL) <span className="text-muted-foreground">(blank = deliver all)</span>
        </Label>
        <textarea
          id="pipeline-filter"
          className={textareaClass}
          value={filterExpr}
          onChange={(e) => setFilterExpr(e.target.value)}
          placeholder={outbound ? 'payload.type == "order.created"' : 'header("x-event") == "push"'}
        />
        <div className="mt-2 flex items-center gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={testing !== null}
            onClick={testFilter}
          >
            {testing === 'filter' ? 'Testing…' : 'Test filter'}
          </Button>
          {filterResult != null && (
            <span
              className={
                'font-mono text-xs ' + (filterOk ? 'text-foreground' : 'text-destructive')
              }
            >
              {filterResult}
            </span>
          )}
        </div>
      </div>

      <div>
        <Label htmlFor="pipeline-transform" className="mb-2 block">
          Transform (JS) <span className="text-muted-foreground">(blank = passthrough)</span>
        </Label>
        <textarea
          id="pipeline-transform"
          className={textareaClass}
          value={transformJs}
          onChange={(e) => setTransformJs(e.target.value)}
          placeholder={'function transform(payload) {\n  return payload\n}'}
        />
        <div className="mt-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={testing !== null}
            onClick={testTransform}
          >
            {testing === 'transform' ? 'Testing…' : 'Test transform'}
          </Button>
          {transformResult != null && (
            <pre
              className={
                'mt-2 overflow-x-auto rounded border px-3 py-2 font-mono text-xs ' +
                (transformOk
                  ? 'border-border bg-muted'
                  : 'border-destructive/40 bg-destructive/10 text-destructive')
              }
            >
              {transformResult}
            </pre>
          )}
        </div>
      </div>

      <div>
        <Label htmlFor="pipeline-sample" className="mb-2 block">
          Sample payload{' '}
          <span className="text-muted-foreground">(JSON — used by both Test buttons)</span>
        </Label>
        <textarea
          id="pipeline-sample"
          className={textareaClass}
          value={sample}
          onChange={(e) => setSample(e.target.value)}
          placeholder="{}"
        />
      </div>
    </>
  )
}
