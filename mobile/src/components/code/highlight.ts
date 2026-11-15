// Code as lines of coloured spans: Prism's tokenizer (Prism.tokenize, a token
// tree, no HTML), its token types folded to the few roles the TUI's glamour
// chroma style colours (theme.ts syntax). Pure: no React, no DOM.
import './prism-manual';
import Prism from 'prismjs';
// Grammars, each after the ones it extends.
import 'prismjs/components/prism-clike';
import 'prismjs/components/prism-markup';
import 'prismjs/components/prism-css';
import 'prismjs/components/prism-javascript';
import 'prismjs/components/prism-typescript';
import 'prismjs/components/prism-jsx';
import 'prismjs/components/prism-tsx';
import 'prismjs/components/prism-go';
import 'prismjs/components/prism-python';
import 'prismjs/components/prism-bash';
import 'prismjs/components/prism-json';
import 'prismjs/components/prism-yaml';
import 'prismjs/components/prism-toml';
import 'prismjs/components/prism-sql';
import 'prismjs/components/prism-rust';
import 'prismjs/components/prism-java';
import 'prismjs/components/prism-kotlin';
import 'prismjs/components/prism-c';
import 'prismjs/components/prism-cpp';
import 'prismjs/components/prism-markdown';
import 'prismjs/components/prism-diff';
import 'prismjs/components/prism-docker';
import 'prismjs/components/prism-makefile';
import 'prismjs/components/prism-graphql';

import type { Lang } from './languages';

/** A colour role (theme.ts SyntaxColors keys). */
export type Role =
  | 'comment'
  | 'keyword'
  | 'type'
  | 'string'
  | 'number'
  | 'function'
  | 'builtin'
  | 'operator'
  | 'punctuation'
  | 'tag'
  | 'attr'
  | 'decorator'
  | 'preproc'
  | 'inserted'
  | 'deleted';

export type Span = { text: string; role?: Role };
export type Line = Span[];

// Prism token type or alias → role. Chroma's names in comments where they differ.
const ROLES: Record<string, Role> = {
  comment: 'comment',
  prolog: 'comment',
  doctype: 'comment',
  cdata: 'comment',
  keyword: 'keyword',
  atrule: 'keyword',
  important: 'keyword',
  boolean: 'keyword', // KeywordConstant
  nil: 'keyword',
  null: 'keyword',
  'class-name': 'type', // KeywordType / NameClass
  'maybe-class-name': 'type',
  string: 'string',
  char: 'string',
  'template-string': 'string',
  'attr-value': 'string',
  url: 'string',
  regex: 'string',
  number: 'number',
  constant: 'number',
  symbol: 'number',
  function: 'function',
  'function-variable': 'function',
  method: 'function',
  builtin: 'builtin',
  operator: 'operator',
  punctuation: 'punctuation',
  tag: 'tag',
  selector: 'tag',
  property: 'tag', // JSON and YAML keys are NameTag
  key: 'tag',
  'attr-name': 'attr',
  decorator: 'decorator',
  annotation: 'decorator',
  macro: 'preproc',
  directive: 'preproc',
  shebang: 'preproc',
  inserted: 'inserted',
  deleted: 'deleted',
};

function roleOf(t: Prism.Token): Role | undefined {
  const own = ROLES[t.type];
  if (own) return own;
  const aliases = typeof t.alias === 'string' ? [t.alias] : (t.alias ?? []);
  for (const a of aliases) if (ROLES[a]) return ROLES[a];
  return undefined;
}

/** Plain lines: what an unknown language, or no grammar, draws. */
export function plainLines(code: string): Line[] {
  return code.split('\n').map((l) => (l ? [{ text: l }] : []));
}

/** code as lines of spans in lang's grammar; null lang → plain. Never throws. */
export function highlight(code: string, lang: Lang | null): Line[] {
  const grammar = lang ? Prism.languages[lang] : undefined;
  if (!grammar) return plainLines(code);
  let stream: Prism.TokenStream;
  try {
    stream = Prism.tokenize(code, grammar);
  } catch {
    return plainLines(code);
  }
  const lines: Line[] = [[]];
  const push = (text: string, role: Role | undefined) => {
    const parts = text.split('\n');
    parts.forEach((p, i) => {
      if (i > 0) lines.push([]);
      if (!p) return;
      const line = lines[lines.length - 1]!;
      const last = line[line.length - 1];
      if (last && last.role === role) last.text += p;
      else line.push(role ? { text: p, role } : { text: p });
    });
  };
  const walk = (s: Prism.TokenStream, role: Role | undefined) => {
    if (typeof s === 'string') push(s, role);
    else if (Array.isArray(s)) for (const t of s) walk(t, role);
    else walk(s.content, roleOf(s) ?? role);
  };
  walk(stream, undefined);
  return lines;
}
