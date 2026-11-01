<script lang="ts">
  import { untrack } from "svelte";

  interface Props {
    value: number | null | undefined;
    format?: (v: number) => string;
    duration?: number;
    class?: string;
  }

  let {
    value,
    format = (v: number) => String(v),
    duration = 480,
    class: className = "",
  }: Props = $props();

  // Intentionally seed from the first real value: no count-up from zero on
  // first paint, only tweening when the value later changes.
  // svelte-ignore state_referenced_locally
  let display = $state<number | null>(value ?? null);
  let raf = 0;

  $effect(() => {
    const target = value;
    if (target == null || !Number.isFinite(target)) {
      display = null;
      return;
    }
    const current = untrack(() => display);
    if (current == null || !Number.isFinite(current)) {
      display = target;
      return;
    }
    if (Math.abs(target - current) < 1e-9) {
      display = target;
      return;
    }
    if (
      typeof window === "undefined" ||
      window.matchMedia("(prefers-reduced-motion: reduce)").matches
    ) {
      display = target;
      return;
    }

    cancelAnimationFrame(raf);
    const from = current;
    const start = performance.now();
    const step = (now: number): void => {
      const p = Math.min(1, (now - start) / duration);
      // Ease-in-out: a measured value should settle, not race away from its
      // previous sample. The old ease-out spent most of the travel in the
      // first third of the tween.
      const eased =
        p < 0.5
          ? 4 * p * p * p
          : 1 - Math.pow(-2 * p + 2, 3) / 2;
      display = from + (target - from) * eased;
      if (p < 1) {
        raf = requestAnimationFrame(step);
      } else {
        display = target;
      }
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  });

  const exact = $derived(value == null ? "—" : format(value));
  const visual = $derived(display == null ? "—" : format(display));
</script>

<span class={className}>
  <span class="sr-only">{exact}</span>
  <span aria-hidden="true">{visual}</span>
</span>
