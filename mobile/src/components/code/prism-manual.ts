// Imported before prismjs: on web, Prism's core would otherwise highlight
// every code[class*=language-] element on DOMContentLoaded. We only call
// Prism.tokenize; it never touches the DOM.
const g = globalThis as { Prism?: { manual?: boolean } };
g.Prism = { ...g.Prism, manual: true };

export {};
