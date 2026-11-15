// Which grammar a fence or a file gets, as the TUI picks a chroma lexer.
// A fence's language (bubbleteandmd fencelang.go, chroma lexers.Get): a name,
// an alias or a file extension, any case, else a file name; anything else is
// plain, never guessed from the content. A file's (bubbleteandmd diff.go
// lexerCache.get): its base name's extension, else the base name itself.

/** The grammars registered in highlight.ts. */
export type Lang =
  | 'bash'
  | 'c'
  | 'cpp'
  | 'css'
  | 'diff'
  | 'docker'
  | 'go'
  | 'graphql'
  | 'java'
  | 'javascript'
  | 'json'
  | 'jsx'
  | 'kotlin'
  | 'makefile'
  | 'markdown'
  | 'markup'
  | 'python'
  | 'rust'
  | 'sql'
  | 'toml'
  | 'tsx'
  | 'typescript'
  | 'yaml';

// Lower-case names, aliases and extensions, chroma's where it has them.
const NAMES: Record<string, Lang> = {
  bash: 'bash',
  sh: 'bash',
  shell: 'bash',
  zsh: 'bash',
  ksh: 'bash',
  console: 'bash',
  'shell-session': 'bash',
  c: 'c',
  h: 'c',
  cpp: 'cpp',
  'c++': 'cpp',
  cc: 'cpp',
  cxx: 'cpp',
  hpp: 'cpp',
  hh: 'cpp',
  hxx: 'cpp',
  css: 'css',
  diff: 'diff',
  patch: 'diff',
  udiff: 'diff',
  docker: 'docker',
  dockerfile: 'docker',
  go: 'go',
  golang: 'go',
  graphql: 'graphql',
  gql: 'graphql',
  graphqls: 'graphql',
  java: 'java',
  javascript: 'javascript',
  js: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  node: 'javascript',
  json: 'json',
  jsonc: 'json',
  json5: 'json',
  webmanifest: 'json',
  jsx: 'jsx',
  kotlin: 'kotlin',
  kt: 'kotlin',
  kts: 'kotlin',
  makefile: 'makefile',
  make: 'makefile',
  mk: 'makefile',
  mf: 'makefile',
  bsdmake: 'makefile',
  markdown: 'markdown',
  md: 'markdown',
  mkd: 'markdown',
  html: 'markup',
  htm: 'markup',
  xhtml: 'markup',
  xml: 'markup',
  svg: 'markup',
  xsl: 'markup',
  plist: 'markup',
  python: 'python',
  py: 'python',
  python3: 'python',
  py3: 'python',
  pyi: 'python',
  starlark: 'python',
  star: 'python',
  bzl: 'python',
  bazel: 'python',
  rust: 'rust',
  rs: 'rust',
  sql: 'sql',
  postgresql: 'sql',
  postgres: 'sql',
  mysql: 'sql',
  sqlite: 'sql',
  sqlite3: 'sql',
  toml: 'toml',
  tsx: 'tsx',
  typescript: 'typescript',
  ts: 'typescript',
  mts: 'typescript',
  cts: 'typescript',
  yaml: 'yaml',
  yml: 'yaml',
};

// Whole file names chroma matches without an extension (lower-case).
const FILES: Record<string, Lang> = {
  dockerfile: 'docker',
  containerfile: 'docker',
  makefile: 'makefile',
  gnumakefile: 'makefile',
  '.bashrc': 'bash',
  '.zshrc': 'bash',
  '.profile': 'bash',
  'build.bazel': 'python',
  workspace: 'python',
};

function baseName(path: string): string {
  const p = path.replace(/\\/g, '/');
  return p.slice(p.lastIndexOf('/') + 1);
}

/** The grammar for a file path, or null (plain). */
export function langForPath(path: string | undefined | null): Lang | null {
  if (!path) return null;
  const base = baseName(path.trim()).toLowerCase();
  if (!base) return null;
  const dot = base.lastIndexOf('.');
  if (dot > 0) {
    const ext = NAMES[base.slice(dot + 1)];
    if (ext) return ext;
  }
  if (FILES[base]) return FILES[base];
  // Dockerfile.dev, Makefile.inc: chroma's globs are Dockerfile* style for these two.
  if (base.startsWith('dockerfile')) return 'docker';
  if (base.startsWith('makefile')) return 'makefile';
  return null;
}

/** The grammar for a fence's info string (its first word), or null (plain). */
export function langOf(info: string | undefined | null): Lang | null {
  const name = (info ?? '').trim().split(/\s+/)[0]?.toLowerCase() ?? '';
  if (!name) return null;
  return NAMES[name] ?? NAMES[name.replace(/^\./, '')] ?? langForPath(name);
}
