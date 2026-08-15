<script lang="ts">
  import FoldCell from "./FoldCell.svelte";
  import type { Frame, Sample } from "./metrics.js";

  // The sheet: a tessellated parallelogram grid of fold-cells under a
  // single isometric skew. `cascadeOn` drives the stepped diagonal deploy
  // animation (toggled off/on to replay it WITHOUT remounting — remounting
  // would reset every trace's scroll window). `folded` collapses every
  // cell to its thin packet row (mobile/compact mode).

  interface Props {
    frame: Frame;
    hist: Record<string, Sample[]>;
    deployed: boolean;
    cascadeOn: boolean;
    open: string | null;
    onToggle: (name: string) => void;
    registerEl: (name: string, el: HTMLElement | null) => void;
  }

  let { frame, hist, deployed, cascadeOn, open, onToggle, registerEl }: Props =
    $props();
</script>

<div
  class="sheet-viewport"
  class:is-deployed={deployed}
  role="region"
  aria-label="fleet fold-cells"
>
  <div class="sheet {deployed ? 'is-deployed' : 'is-folded'}" class:cascade={cascadeOn}>
    {#each frame.backends as b (b.name)}
      {@const idx = frame.backends.indexOf(b)}
      <FoldCell
        backend={b}
        hist={hist[b.name] ?? []}
        order={idx}
        open={open === b.name}
        onToggle={() => onToggle(b.name)}
        registerEl={registerEl}
      />
    {/each}
  </div>
</div>
