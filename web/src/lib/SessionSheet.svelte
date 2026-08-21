<script lang="ts">
  import PreFillCard from "./PreFillCard.svelte";
  import { clock, fmtCompact } from "./fmt.js";
  import type { SessionsFeed } from "./metrics.js";

  // The sessions view: the router's live in-flight stack (oldest on top)
  // and the conversation sessions, expandable to per-request rows. Every
  // token figure is a body-based estimate — the gold "est" label rides on
  // all of them. 429 = valley blue (capacity policy working), 5xx = red.
  // The 1s wall clock (the elapsed/age columns and the prefill bars tick)
  // is owned by the page and passed in — one timer per sheet.

  interface Props {
    feed: SessionsFeed | null;
    now: number;
  }

  let { feed, now }: Props = $props();

  let openId: string | null = $state(null);

  // elapsed: under a minute reads s.s, after that m:ss
  function elapsed(ms: number): string {
    const s = Math.max(0, ms) / 1000;
    if (s < 60) return s.toFixed(1) + "s";
    const m = Math.floor(s / 60);
    return `${m}:${String(Math.floor(s % 60)).padStart(2, "0")}`;
  }

  // session age: m:ss (live sessions show the chip instead)
  function age(ms: number): string {
    const s = Math.max(0, Math.floor(ms / 1000));
    const m = Math.floor(s / 60);
    return `${m}:${String(s % 60).padStart(2, "0")}`;
  }

  function stClass(status: number): string {
    if (status === 429) return "st-throttle";
    if (status >= 400) return "st-err";
    return "st-ok";
  }
</script>

<section class="sessions" aria-label="sessions">
  <div class="unskew">
    <div class="sess-head">
      <span class="sess-label">sessions</span>
      <span class="sess-inflight" class:hot={feed !== null && feed.live.length > 0}>
        in flight · {feed?.live.length ?? 0}
      </span>
      <span class="sess-est-note">
        token figures · <em>est</em>
      </span>
    </div>

    {#if feed === null}
      <div class="sess-hollow">no session feed</div>
    {:else}
      {#if feed.live.length > 0}
        <PreFillCard live={feed.live} now={now} />
        <div class="sess-subhead">in flight — live</div>
        {#each feed.live as r (r.id)}
          <div class="sess-live sess-row">
            <span class="sess-elapsed">{elapsed(now - r.startMs)}</span>
            <span class="sess-be">{r.backend}</span>
            <span class="sess-path">{r.path}</span>
            <span class="sess-sess">{r.sess === "" ? "—" : r.sess}</span>
            <span class="sess-phase sess-phase-{r.phase}">{r.phase}</span>
            <span class="sess-tok">
              ctx {r.ctxTok} <em class="est">est</em>
            </span>
            <span class="sess-tok">
              new {r.newTok} <em class="est">est</em>
            </span>
          </div>
        {/each}
      {/if}

      {#if feed.sessions.length > 0}
        <div class="sess-scroll">
          {#each feed.sessions as s (s.id)}
            <div class="sess-wrap {s.active ? 'sess-active' : ''}">
              <button
                class="sess-row"
                aria-expanded={openId === s.id}
                onclick={() => (openId = openId === s.id ? null : s.id)}>
                <span class="sess-id">{s.id}</span>
                <span class="sess-be">{s.backend}</span>
                <span class="sess-n">{s.n} reqs</span>
                {#if s.active}
                  <span class="sess-live-chip">live</span>
                {:else}
                  <span class="sess-age">{age(now - s.lastMs)}</span>
                {/if}
                <span class="sess-tok">
                  ctx {s.ctxTok} <em class="est">est</em>
                </span>
                <span class="sess-tok">
                  cached {s.cachedTok} <em class="est">est</em>
                </span>
                <span class="sess-ttft">ttft {s.avgTTFTMs > 0 ? Math.round(s.avgTTFTMs) : "—"}ms</span>
                <span class="sess-tok">
                  {s.tokOutTotal} tok <em class="est">est</em>
                </span>
                <span class="sess-st {stClass(s.lastStatus)}">{s.lastStatus}</span>
              </button>
              {#if openId === s.id}
                <div class="sess-detail">
                  {#if s.reqs}
                    <table class="sess-reqs">
                      <thead>
                        <tr>
                          <th>t</th>
                          <th>st</th>
                          <th>dur</th>
                          <th>ttft</th>
                          <th>ctx est</th>
                          <th>new est</th>
                          <th>cached est</th>
                          <th>out est</th>
                          <th>tok/s est</th>
                        </tr>
                      </thead>
                      <tbody>
                        {#each s.reqs as r (r.tMs + "-" + r.status)}
                          <tr>
                            <td>{clock(r.tMs)}</td>
                            <td class={stClass(r.status)}>{r.status}</td>
                            <td>{r.durMs >= 1000 ? (r.durMs / 1000).toFixed(1) + "s" : r.durMs.toFixed(0) + "ms"}</td>
                            <td>{r.ttftMs > 0 ? r.ttftMs.toFixed(0) : "—"}</td>
                            <td>{r.ctxTok}</td>
                            <td>{r.newTok}</td>
                            <td>{r.cachedTok}</td>
                            <td>{r.tokOut}</td>
                            <td>{r.tokS > 0 ? fmtCompact(r.tokS) : "—"}</td>
                          </tr>
                        {/each}
                      </tbody>
                    </table>
                  {:else}
                    <div class="sess-no-reqs">request detail available while active</div>
                  {/if}
                </div>
              {/if}
            </div>
          {/each}
        </div>
      {:else}
        <div class="sess-hollow">no conversations yet</div>
      {/if}
    {/if}
  </div>
</section>
