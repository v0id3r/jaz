export function isIOS(): boolean {
  return /iPhone|iPad|iPod/.test(navigator.platform) || navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1
}
