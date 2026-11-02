import { spawnSync } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const helpers = [
  {
    output: 'jaz-dictation',
    args: [
      'native/dictation/main.swift',
      'native/dictation/audio.swift',
      '-Xlinker', '-sectcreate', '-Xlinker', '__TEXT', '-Xlinker', '__info_plist',
      '-Xlinker', 'native/dictation/Info.plist',
    ],
  },
  { output: 'jaz-device-key', args: ['native/device-key/main.swift'] },
]

if (process.platform === 'darwin') {
  mkdirSync(resolve(root, 'resources/bin'), { recursive: true })
  for (const helper of helpers) {
    const result = spawnSync('xcrun', [
      'swiftc', '-parse-as-library', '-O', '-target', `${process.arch === 'arm64' ? 'arm64' : 'x86_64'}-apple-macos13.0`,
      ...helper.args, '-o', `resources/bin/${helper.output}`,
    ], { cwd: root, stdio: 'inherit' })
    if (result.error) {
      throw result.error
    }
    if (result.status !== 0) {
      process.exit(result.status ?? 1)
    }
  }
}
