import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { useAutoRefresh } from "../hooks/useAutoRefresh";
import { Badge } from "./ui/badge";

export function useAPIKeyConcurrency() {
  const [counts, setCounts] = useState<Record<string, number> | null>(null);
  const refresh = async () => {
    const response = await api.getAPIKeyConcurrency();
    setCounts(response.concurrency);
  };
  useEffect(() => { void refresh().catch(() => undefined); }, []);
  useAutoRefresh(refresh, 60_000, true, { topics: ["runtime", "keys"], minEventIntervalMs: 1000 });
  return counts;
}

export default function APIKeyConcurrencyBadge({ current, limit }: {
  current?: number;
  limit?: number;
}) {
  const { t } = useTranslation();
  if (current === undefined || current <= 0) return null;

  return (
    <Badge
      variant="outline"
      title={t("apiKeys.limits.concurrencySuffix")}
      className="gap-1.5 rounded-full border-blue-500/20 bg-blue-50 px-2 py-0.5 text-[11px] font-semibold tabular-nums text-blue-600 shadow-2xs dark:border-blue-400/20 dark:bg-blue-950 dark:text-blue-400"
    >
      <span
        className="size-1.5 animate-pulse rounded-full bg-blue-500 dark:bg-blue-400"
        aria-hidden
      />
      {current}/{limit && limit > 0 ? limit : "∞"}
    </Badge>
  );
}
