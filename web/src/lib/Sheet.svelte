<script lang="ts">
  import FoldCell from "./FoldCell.svelte";
  import type { Frame, Sample } from "./metrics.js";

  // The sheet: a tessellated parallelogram grid of fold-cells under a
  // single isometric skew. `cascade` replays the stepped diagonal deploy;
  // `folded` collapses every cell to its thin packet row (mobile/compact
  // mode — the same state the [FOLD] control drives).

  interface Props {
    frame: Frame;
    hist: Record<string, Sample[]>;
    deployed: boolean;
    open: string | null;
    onToggle: (name: string) => void;
  }

  let { frame, hist, deployed, open, onToggle }: Props = $props();
</script>

<div
  class="sheet-viewport"
  class:is-deployed={deployed}
  role="region"
  aria-label="fleet fold-cells"
>
  <div class="sheet cascade {deployed ? 'is-deployed' : 'is-folded'}">
    {#each frame.backends as b (b.name)}
      {@const idx = frame.backends.indexOf(b)}
      <FoldCell
        backend={b}
        hist={hist[b.name] ?? []}
        order={idx}
        open={open === b.name}
        onToggle={() => onToggle(b.name)}
      />
    {/each}
  </div>
</div>
