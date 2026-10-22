import type { CodegenConfig } from '@graphql-codegen/cli';

// The typed GraphQL client (WEB-4), generated from the committed SDL snapshot
// (schema.graphql), so `pnpm gen` needs no running service. After a change to
// dund's operations: `dun dund schema > mobile/schema.graphql`, then
// `pnpm gen`. Output lands in src/gql/ (gitignored). Mirrors veliode-go/ui.
const config: CodegenConfig = {
  overwrite: true,
  schema: './schema.graphql',
  documents: ['src/**/*.{ts,tsx}'],
  generates: {
    './src/gql/': {
      preset: 'client',
      config: {
        useTypeImports: true,
        // gat emits Long (int64) as JSON strings; map to string in/out.
        scalars: { Long: { input: 'string', output: 'string' }, JSON: 'unknown' },
        enumsAsTypes: true,
        // Query strings (TypedDocumentString), not parsed AST: keeps the
        // `graphql` parser out of the bundle. graphql-request@7 takes them.
        documentMode: 'string',
      },
    },
  },
  ignoreNoDocuments: true,
};

export default config;
