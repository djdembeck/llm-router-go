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

<div class="stale" role="alert">
  <span class="mark" aria-hidden="true"></span>
  <span class="text">
    <strong>STALE</strong> — feed interrupted; last data
    <span class="mono">{lastFrameAt ? fmtClock(lastFrameAt) : "—"}</span>
    <span class="mono">· {fmtSince(age)} ago; holding the last frame</span>
  </span>
  <span class="right mono muted">re-arming {feedName}</span>
</div>

<style>
  .stale {
    animation: banner-in var(--dur) var(--ease-out);
    display: flex;
    align-items: center;
    gap: var(--s3);
    min-height: 34px;
    padding: 6px var(--s5);
    background: var(--panel);
    border-bottom: 1px solid var(--verm);
    color: var(--verm);
    font-size: var(--fs-detail);
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
  }
</style>
