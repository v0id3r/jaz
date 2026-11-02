import { execFileSync, spawn } from 'node:child_process'
import { createInterface } from 'node:readline'

type Pending = { resolve: (signature: Uint8Array) => void; reject: (error: Error) => void }

// Creates a P-256 key inside the Secure Enclave. The handle only lets this
// Mac's enclave use the key; the key itself never leaves it.
export function createEnclaveKey(helper: string): { handle: string; publicKey: Buffer } {
  const { handle, public_key } = JSON.parse(execFileSync(helper, ['create'], { encoding: 'utf8', timeout: 10_000 })) as { handle: string; public_key: string }
  return { handle, publicKey: Buffer.from(public_key, 'base64') }
}

// DPoP signs every request, so one helper process serves all signatures and is
// restarted only after it exits. Answers arrive in request order.
export function enclaveSigner(helper: string, handle: string): (input: Uint8Array) => Promise<Uint8Array> {
  let running: { write: (line: string) => void; pending: Pending[] } | undefined
  const start = () => {
    const child = spawn(helper, ['serve'], { stdio: ['pipe', 'pipe', 'ignore'] })
    const state = { write: (line: string) => child.stdin.write(line + '\n'), pending: [] as Pending[] }
    const stop = () => {
      if (running === state) running = undefined
      for (const request of state.pending.splice(0)) request.reject(new Error('The Secure Enclave device key is unavailable'))
    }
    child.on('error', stop)
    child.on('exit', stop)
    child.stdin.on('error', stop)
    createInterface({ input: child.stdout }).on('line', (line) => {
      const request = state.pending.shift()
      if (line === 'error') request?.reject(new Error('The Secure Enclave could not sign'))
      else request?.resolve(new Uint8Array(Buffer.from(line, 'base64')))
    })
    state.write(handle)
    return state
  }
  return (input) => new Promise((resolve, reject) => {
    running ??= start()
    running.pending.push({ resolve, reject })
    running.write(Buffer.from(input).toString('base64'))
  })
}
