<script lang="ts">
  import { RANGES, type RangeName } from "./api";
  import { fmtClock } from "./format";

  let {
    health,
    feedName,
    mock,
    lastFrameAt,
    paused,
    range,
    now,
    theme,
    onRange,
    onPause,
    onTheme,
  }: {
    health: "live" | "degraded" | "offline";
    feedName: string;
    mock: boolean;
    lastFrameAt: number | null;
    paused: boolean;
    range: RangeName;
    now: number;
    theme: "night" | "day";
    onRange: (r: RangeName) => void;
    onPause: () => void;
    onTheme: () => void;
  } = $props();

  const healthLabel = $derived(
    health === "live" ? "LIVE" : health === "degraded" ? "DEGRADED" : "OFFLINE",
  );
</script>

<header class="topbar">
  <div class="mark">
    <span class="wordmark">llm-router<span class="suffix">-go</span></span>
    <span class="label subtitle">GPU-aware inference routing</span>
  </div>

  <div class="status">
    <span class="chip {health === 'live' ? 'ok' : health === 'degraded' ? 'warn' : 'err'}">
      {healthLabel}
    </span>
    {#if paused}
      <span class="chip warn">PAUSED</span>
    {/if}
    {#if mock}
      <span class="chip warn" title="dev-only synthetic feed; not the router">DEV MOCK</span>
    {/if}
    <span class="feed detail muted">
      {feedName}
      {#if lastFrameAt}
        <span class="sep">·</span>
        last data <span class="mono">{fmtClock(lastFrameAt)}</span>
      {/if}
    </span>
  </div>

  <div class="controls">
    <span class="clock mono" aria-label="wall clock">{fmtClock(now)}</span>
    <div class="seg" role="group" aria-label="history range">
      {#each RANGES as r (r.id)}
        <button
          class="btn"
          aria-pressed={range === r.id}
          title={r.long}
          onclick={() => onRange(r.id)}
        >
          {r.label}
        </button>
      {/each}
    </div>
    <button
      class="btn"
      aria-pressed={paused}
      onclick={onPause}
      aria-label={paused ? "resume live updates" : "pause live updates"}
    >
      {#if paused}
        <svg class="icon" viewBox="0 0 12 12" aria-hidden="true"><path d="M3 2l7 4-7 4z" /></svg>
        RESUME
      {:else}
        <svg class="icon" viewBox="0 0 12 12" aria-hidden="true"><path d="M3 2v8M9 2v8" /></svg>
        PAUSE
      {/if}
    </button>
    <button
      class="btn"
      onclick={onTheme}
      aria-label={theme === "night" ? "switch to day sheet" : "switch to night sheet"}
      title={theme === "night" ? "day sheet" : "night sheet"}
    >
      {#if theme === "night"}
        <svg class="icon" viewBox="0 0 12 12" aria-hidden="true">
          <circle cx="6" cy="6" r="2.5" />
          <path d="M6 1v1.5M6 9.5V11M1 6h1.5M9.5 6H11M2.5 2.5l1 1M8.5 8.5l1 1M9.5 2.5l-1 1M3.5 8.5l-1 1" />
        </svg>
      {:else}
        <svg class="icon" viewBox="0 0 12 12" aria-hidden="true">
          <path d="M9.5 7.5A4 4 0 0 1 4.5 2.5a4.5 4.5 0 1 0 5 5z" />
        </svg>
      {/if}
      {theme === "night" ? "DAY" : "NIGHT"}
    </button>
  </div>
</header>

<style>
  .topbar {
    display: flex;
    align-items: center;
    gap: var(--s5);
    min-height: 56px;
    padding: 8px var(--s5);
    border-bottom: 1px solid var(--rule-2);
    background: var(--panel);
    transition:
      background-color var(--dur) var(--ease-soft),
      border-color var(--dur) var(--ease-soft);
  }
  .mark {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 200px;
  }
  .wordmark {
    font-size: var(--fs-title);
    line-height: var(--lh-body);
    font-weight: 600;
    letter-spacing: -0.01em;
  }
  .suffix {
    color: var(--sand);
  }
  .subtitle {
    color: var(--ink-3);
    letter-spacing: 0.1em;
  }
  .status {
    display: flex;
    align-items: center;
    gap: var(--s3);
    min-width: 0;
  }
  .feed {
    white-space: nowrap;
  }
  .controls {
    display: flex;
    align-items: center;
    gap: var(--s2);
    margin-left: auto;
  }
  .clock {
    font-size: var(--fs-detail);
    color: var(--ink-2);
    padding-right: var(--s2);
  }
  .sep {
    color: var(--ink-3);
    padding: 0 4px;
  }
  @media (max-width: 1100px) {
    .topbar {
      flex-wrap: wrap;
      gap: var(--s3);
    }
    .status {
      order: 3;
      flex-basis: 100%;
    }
    .controls {
      order: 2;
    }
    .clock {
      display: none;
    }
  }
  @media (max-width: 639px) {
    .topbar {
      padding: 8px var(--s3);
    }
    .mark {
      min-width: 0;
    }
    .subtitle {
      display: none;
    }
    .controls {
      width: 100%;
      overflow-x: auto;
      padding-bottom: 2px;
    }
    .seg .btn {
      min-height: 30px;
      padding: 4px 9px;
    }
    .feed {
      white-space: normal;
      font-size: var(--fs-label);
    }
  }
</style>
