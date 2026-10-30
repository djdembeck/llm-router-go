<script lang="ts">
  import { fmtClock, fmtSince } from "./format";

  let {
    lastFrameAt,
    now,
    feedName,
  }: {
    lastFrameAt: number | null;
    now: number;
    feedName: string;
  } = $props();

  const age = $derived(lastFrameAt ? now - lastFrameAt : 0);
</script>

<div class="stale-wrap">
  <div class="stale" role="alert">
    <span class="mark" aria-hidden="true"></span>
    <span class="text">
      <strong>STALE</strong> — feed interrupted; last data
      <span class="mono">{lastFrameAt ? fmtClock(lastFrameAt) : "—"}</span>;
      holding the last frame
    </span>
    <span class="right mono muted">re-arming {feedName}</span>
  </div>
  <!-- The ticking age lives outside the assertive region: a role=timer
       sibling that AT tracks by its own (opt-in) means, never re-announced
       as a whole-region alert every second. -->
  <span class="age mono muted" role="timer" aria-label="stale feed age">
    {lastFrameAt ? `${fmtSince(age)} ago` : ""}
  </span>
</div>

<style>
  .stale-wrap {
    display: flex;
    align-items: center;
    flex-wrap: nowrap;
    background: var(--panel);
    border-bottom: 1px solid var(--verm);
  }
  .stale {
    animation: banner-in var(--dur) var(--ease-out);
    display: flex;
    align-items: center;
    gap: var(--s3);
    min-height: 34px;
    padding: 6px var(--s5);
    color: var(--verm);
    font-size: var(--fs-detail);
  }
  .age {
    padding-right: var(--s5);
    white-space: nowrap;
  }
  .mark {
    width: 8px;
    height: 8px;
    background: var(--verm);
    flex: none;
  }
  strong {
    letter-spacing: 0.1em;
  }
  .text {
    color: var(--ink-2);
  }
  .right {
    margin-left: auto;
    white-space: nowrap;
  }
  @keyframes banner-in {
    from {
      opacity: 0;
      transform: translateY(-4px);
    }
    to {
      opacity: 1;
      transform: none;
    }
  }

  @media (max-width: 639px) {
    .stale {
      padding: 6px var(--s3);
      align-items: flex-start;
      flex-wrap: wrap;
    }
    .right {
      margin-left: 0;
      width: 100%;
    }
    .age {
      padding-right: var(--s3);
    }
  }
</style>
