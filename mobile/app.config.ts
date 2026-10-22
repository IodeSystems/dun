import type { ConfigContext, ExpoConfig } from 'expo/config';

// app.json holds the static config. A release stamps itself through the env
// (as bintag's bin/release-android does):
//   DUN_VERSION_CODE -> android.versionCode (must strictly increase between releases)
//   DUN_VERSION_NAME -> version (Android versionName), e.g. "0.1.0+ab12cd3"
// Unset (dev, expo start) leaves app.json as it is.
export default ({ config }: ConfigContext): ExpoConfig => {
  const code = process.env.DUN_VERSION_CODE;
  const name = process.env.DUN_VERSION_NAME;
  if (code !== undefined && !/^[1-9][0-9]*$/.test(code)) {
    throw new Error(`DUN_VERSION_CODE must be a positive integer, got "${code}"`);
  }
  return {
    ...(config as ExpoConfig),
    ...(name ? { version: name } : {}),
    android: {
      ...config.android,
      ...(code ? { versionCode: Number(code) } : {}),
    },
  };
};
