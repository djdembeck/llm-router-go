<script lang="ts">
  import Trace from "./Trace.svelte";
  import { fmtCompact } from "./fmt.js";
  import type { Frame, Sample } from "./metrics.js";

  // Engine pools band: the backends' memory pools, read from the engines'
  // own /metrics — kv-cache pressure per backend, the SGLang full/SWA/mamba
  // split, prefix-cache hit rate, pool token counts, and a 5-minute scope
  // of the kv pressure. Everything here is MEASURED by the engine (valley
  // "real", never est, and gold never appears in this band — gold is the
  // router's live signal, and these gauges are the engine's, not the
  // router's). Family differences are silent: a vLLM backend simply has no
  // mamba row; a pool the engine does not report renders as —. When no
  // backend reports /metrics the band keeps its shell and says so in one
  // muted line — absence is not failure, so it is never red.

  interface Props {
    frame: Frame;
    hist: Record<string, Sample[]>;
  }

  let { frame, hist }: Props = $props();

  const hasPools = $derived(
    frame.backends.some((b) => b.engine?.status === "ok"),
  );
</script>

<div class="cache" role="group" aria-label="engine memory pools">
  <div class="cache-head unskew">
    <span>engine pools · kv / mamba cache <span class="real">real</span></span>
    <span class="cache-src"
      >scraped from the backends' own /metrics — the engines' gauges, not the
      router's estimate</span
    >
  </div>
  {#if hasPools}
    <div class="cache-grid">
      {#each frame.backends as b (b.name)}
        {@const eng = b.engine}
        {@const engOk = eng.status === "ok"}
        {@const kv = engOk && eng.kvPct > 0 ? eng.kvPct : null}
        {@const held =
          engOk && eng.kvHeldPct !== null
            ? (eng.kvHeldPct / 100) * (100 - (kv ?? 0))
            : null}
        {@const kvTotal = kv !== null || held !== null ? (kv ?? 0) + (held ?? 0) : null}
        {@const mamba = engOk && eng.mambaPct !== null ? eng.mambaPct : null}
        {@const samples = hist[b.name] ?? []}
        <div class="cache-cell unskew">
          <div class="cc-top">
            <span class="cc-name" title={b.name}>{b.name}</span>
            <span class="cc-eng">{engOk ? eng.engine || "engine" : "—"}</span>
          </div>
          {#if engOk}
            <div class="cc-pools">
              <div class="pool">
                <span class="pool-label">kv</span>
                <span
                  class="pool-bar"
                  role="img"
                  aria-label={
                    kvTotal === null
                      ? "kv cache no data"
                      : held !== null
                        ? `kv cache ${Math.round(kv ?? 0)}% used, ${Math.round(held)}% held (cached)`
                        : `kv cache ${Math.round(kv ?? 0)}% used (incl. cached)`
                  }>
                  <i
                    class="seg {kvTotal !== null && kvTotal >= 90 ? 'hot' : ''}"
                    style:transform={kv !== null ? `scaleX(${kv / 100})` : "scaleX(0)"}></i
                  >
                  {#if held !== null}
                    <i class="seg held {kvTotal !== null && kvTotal >= 90 ? 'hot' : ''}" style:left={`${kv ?? 0}%`} style:transform={`scaleX(${held / 100})`}></i>
                  {/if}
                </span>
                <span class="pool-val {kvTotal !== null && kvTotal >= 90 ? 'hot' : ''}"
                  >{kvTotal !== null
                    ? Math.round(kvTotal) + "%"
                    : "—"}{#if held !== null && held > 0 && kvTotal !== null}<span
                      class="held-sfx"> · held {Math.round(held)}%</span
                    >{/if}</span
                >
              </div>
              {#if eng.swaPct !== null}
                <div class="pool">
                  <span class="pool-label">swa</span>
                  <span class="pool-bar" role="img"
                    ><i
                      class="seg {eng.swaPct >= 90 ? 'hot' : ''}"
                      style:transform={`scaleX(${eng.swaPct / 100})`}></i
                    ></span
                  >
                  <span class="pool-val {eng.swaPct >= 90 ? 'hot' : ''}"
                    >{Math.round(eng.swaPct)}%</span
                  >
                </div>
              {/if}
              {#if mamba !== null}
                <div class="pool">
                  <span class="pool-label">mamba</span>
                  <span
                    class="pool-bar"
                    role="img"
                    aria-label="mamba pool {mamba !== null ? Math.round(mamba) + '% used' : 'no data'}">
                    <i
                      class="seg {mamba >= 90 ? 'hot' : ''}"
                      style:transform={`scaleX(${mamba / 100})`}></i
                  ></span>
                  <span class="pool-val {mamba >= 90 ? 'hot' : ''}"
                    >{eng.mambaUsedTok !== null && eng.mambaCapTok !== null
                        ? Math.round(eng.mambaUsedTok) +
                          "/" +
                          Math.round(eng.mambaCapTok)
                      : Math.round(mamba) + "%"}</span
                  >
                </div>
              {/if}
            </div>
            <div class="cc-foot">
              <span>hit <b>{eng.hitRate > 0 ? (eng.hitRate * 100).toFixed(0) + "%" : "—"}</b></span>
              {#if eng.kvUsedTok !== null && eng.kvCapTok !== null}
                <span
                  >pool <b>{fmtCompact(eng.kvUsedTok)}/{fmtCompact(eng.kvCapTok)}</b></span
                >
              {/if}
              {#if held !== null && eng.kvEvictTok !== null && eng.kvEvictTok > 0}
                <span>held <b>{fmtCompact(eng.kvEvictTok)}</b></span>
              {/if}
              {#if held === null && eng.kvEvictTok !== null && eng.kvEvictTok > 0}
                <span>evict <b>{fmtCompact(eng.kvEvictTok)}</b></span>
              {/if}
            </div>
            <div class="cc-trace">
              <Trace
                samples={samples.map((s) => s.kvPct)}
                ts={samples.map((s) => s.t)}
                color="valley"
                yMax={100}
                windowS={300}
              />
            </div>
          {:else}
            <div class="cc-off">
              no /metrics{eng.status === "err" ? " — unreachable" : ""}
            </div>
          {/if}
        </div>
      {/each}
    </div>
  {:else}
    <div class="cache-off unskew">
      no engine pools — the backends do not report /metrics
    </div>
  {/if}
</div>
