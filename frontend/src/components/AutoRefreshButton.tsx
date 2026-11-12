import { useState } from "react";
import { useAutoRefresh } from "../hooks/useAutoRefresh";
import { Check, ChevronDown, RefreshCw } from "lucide-react";
import { DropdownMenu } from "radix-ui";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

// 刷新分裂按钮：左侧手动刷新，右侧下拉选择自动刷新间隔（0 = 关闭）。
// WS 变更通知驱动自动刷新；所选间隔控制最多刷新频率，0 表示暂停。
export function AutoRefreshButton({
  onRefresh,
  onAutoRefresh,
  intervals,
  loadSeconds,
  saveSeconds,
  label,
}: {
  onRefresh: () => void;
  onAutoRefresh: () => Promise<unknown>;
  // 可选间隔（秒），应包含 0 作为「关闭」。
  intervals: readonly number[];
  loadSeconds?: () => number;
  saveSeconds?: (seconds: number) => void;
  label?: string;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [seconds, setSeconds] = useState(() => {
    const initial = loadSeconds?.() ?? 0;
    return intervals.includes(initial) ? initial : 0;
  });
  const [running, setRunning] = useState(false);
  useAutoRefresh(async () => {
    setRunning(true);
    try { await onAutoRefresh(); }
    finally { setRunning(false); }
  }, Math.max(seconds, 60) * 1000, seconds > 0, {
    topics: ["accounts", "usage"],
    minEventIntervalMs: seconds * 1000,
  });

  const resolvedLabel = label ?? t("common.refresh");
  const autoLabel =
    seconds > 0
      ? running
        ? t("common.autoRefreshRunning")
        : t("common.liveUpdates")
      : t("common.autoRefreshOff");

  return (
    <div className="inline-flex shrink-0 items-center">
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => {
          onRefresh();
        }}
        className="rounded-r-none"
        title={seconds > 0 ? `${resolvedLabel} · ${autoLabel}` : resolvedLabel}
      >
        <RefreshCw className={cn("size-3.5", running && "animate-spin")} />
        <span className="max-[380px]:hidden">{resolvedLabel}</span>
        {seconds > 0 ? (
          // 固定标记宽度，更新时按钮不跳动。
          <span
            aria-hidden="true"
            className="min-w-[2.25rem] rounded bg-primary/10 px-1 text-center text-[11px] font-semibold tabular-nums text-primary"
          >
            {running ? "···" : "WS"}
          </span>
        ) : null}
      </Button>
      <DropdownMenu.Root open={open} onOpenChange={setOpen} modal={false}>
        <DropdownMenu.Trigger asChild>
          <Button
            type="button"
            variant="outline"
            size="sm"
            aria-label={t("common.autoRefresh")}
            title={t("common.autoRefresh")}
            className={cn(
              "-ml-px rounded-l-none px-1.5",
              open && "border-primary/25 bg-accent text-accent-foreground",
            )}
          >
            <ChevronDown
              className={cn(
                "size-3.5 text-muted-foreground transition-transform",
                open && "rotate-180",
              )}
            />
          </Button>
        </DropdownMenu.Trigger>
        <DropdownMenu.Portal>
          <DropdownMenu.Content
            data-slot="action-menu-popover"
            align="end"
            sideOffset={8}
            collisionPadding={12}
            className="action-menu-surface z-[200] w-44 rounded-xl border border-border/80 bg-popover p-1.5 text-popover-foreground shadow-[0_12px_40px_-12px_hsl(222_30%_12%/0.3)] outline-none"
          >
            <DropdownMenu.Label className="px-2.5 pb-1 pt-1.5 text-[10px] font-semibold tracking-wide text-muted-foreground">
              {t("common.autoRefresh")}
            </DropdownMenu.Label>
            <DropdownMenu.RadioGroup
              value={String(seconds)}
              onValueChange={(value) => {
                const next = Number(value);
                if (!intervals.includes(next)) return;
                setSeconds(next);
                saveSeconds?.(next);
              }}
            >
              {intervals.map((value) => (
                <DropdownMenu.RadioItem
                  key={value}
                  value={String(value)}
                  className="flex min-h-8 cursor-default select-none items-center gap-2 rounded-lg px-2.5 py-1.5 text-[13px] text-foreground outline-none transition-colors data-[highlighted]:bg-accent"
                >
                  <span className="flex size-4 shrink-0 items-center justify-center">
                    <DropdownMenu.ItemIndicator>
                      <Check className="size-3.5 text-primary" />
                    </DropdownMenu.ItemIndicator>
                  </span>
                  <span className="min-w-0 flex-1">
                    {value > 0
                      ? t("common.liveUpdateInterval", { seconds: value })
                      : t("common.autoRefreshOff")}
                  </span>
                </DropdownMenu.RadioItem>
              ))}
            </DropdownMenu.RadioGroup>
          </DropdownMenu.Content>
        </DropdownMenu.Portal>
      </DropdownMenu.Root>
    </div>
  );
}
