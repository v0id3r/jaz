import { ipcMain, safeStorage } from 'electron'
import { trustedWindow } from '@main/permissions'
import { keychainAvailable } from '@main/keychain'

export function installCredentialEncryption(): void {
  ipcMain.on('jaz:credential-encryption', (event, operation: unknown, value: unknown) => {
    try {
      trustedWindow(event)
      if (typeof value !== 'string' || value.length > 16_384 || (operation !== 'encrypt' && operation !== 'decrypt')) {
        throw new Error('Invalid credential request')
      }
      if (!keychainAvailable()) {
        throw new Error('Saved connections require an available system keychain')
      }
      event.returnValue = { value: operation === 'encrypt'
        ? safeStorage.encryptString(value).toString('base64')
        : safeStorage.decryptString(Buffer.from(value, 'base64')) }
    } catch (error) {
      event.returnValue = { error: (error as Error).message }
    }
  })
}
