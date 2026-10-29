import { safeStorage } from 'electron'

// Linux without a secret service only obfuscates with a hardcoded key, which is
// no keychain at all.
export function keychainAvailable(): boolean {
  return safeStorage.isEncryptionAvailable() && !(process.platform === 'linux' && safeStorage.getSelectedStorageBackend() === 'basic_text')
}
