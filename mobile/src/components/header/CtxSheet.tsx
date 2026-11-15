import { Text, View } from 'react-native';

import { sections, type Ctx, type DetailRow } from '@/lib/ctxstats';
import { noCtxView, type CtxView } from '@/lib/ctxview';
import { pieces, type Stats } from '@/lib/stats';
import { font, mono, space, useColors } from '@/lib/theme';

import { ContextSection } from './ContextSection';
import { Sheet } from './Sheet';

/**
 * The context in detail, opened from the header's ctx: the request figures
 * (lib/stats.ts, the TUI's top bar) and the /context and /usage figures
 * (lib/ctxstats.ts). Labels left, values right-aligned in one column (DES-7).
 */
export function CtxSheet({
  stats,
  ctx,
  ctxv = noCtxView,
  blocks = 0,
  onClose,
}: {
  stats: Stats;
  ctx: Ctx;
  ctxv?: CtxView;
  blocks?: number;
  onClose: () => void;
}) {
  const c = useColors();
  const req: DetailRow[] = stats.seen ? pieces(stats).map((p) => ({ label: p.label, value: p.value.trim() })) : [];
  const all = [{ title: 'this request', rows: req }, ...sections(ctx)].filter((s) => s.rows.length);
  return (
    <Sheet title="Context" onClose={onClose}>
      {!stats.seen && !ctx.seen ? (
        <Text style={{ color: c.muted, fontSize: font.sm }}>
          No request yet: the figures come with the first reply.
        </Text>
      ) : null}
      {all.map((s) => (
        <View key={s.title} style={{ gap: space.xs }}>
          <Text style={{ color: c.text, fontSize: font.sm, fontWeight: '600' }}>{s.title}</Text>
          {s.rows.map((r) => (
            <View key={r.label} style={{ flexDirection: 'row', gap: space.sm, alignItems: 'baseline' }}>
              <Text style={{ flex: 1, color: c.muted, fontSize: font.sm }} numberOfLines={1}>
                {r.label}
              </Text>
              <View style={{ alignItems: 'flex-end', flexShrink: 1 }}>
                <Text style={{ color: c.text, fontSize: font.sm, fontFamily: mono, textAlign: 'right' }}>
                  {r.value}
                </Text>
                {r.note ? (
                  <Text style={{ color: c.muted, fontSize: font.sm, textAlign: 'right' }}>{r.note}</Text>
                ) : null}
              </View>
            </View>
          ))}
        </View>
      ))}
      <ContextSection ctxv={ctxv} blocks={blocks} />
    </Sheet>
  );
}
