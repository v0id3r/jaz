import CryptoKit
import Foundation

// Keeps the Jaz device key in the Secure Enclave. `create` prints a new key's
// opaque handle and raw P-256 public key; `serve` reads a handle, then answers
// each base64 line with the base64 raw (r||s) ECDSA signature, or "error".
@main
struct DeviceKey {
    static func main() {
        setvbuf(stdout, nil, _IOLBF, 0)
        switch CommandLine.arguments.dropFirst().first ?? "" {
        case "create": create()
        case "serve": serve()
        default: fail("usage: jaz-device-key create|serve")
        }
    }

    static func create() {
        guard SecureEnclave.isAvailable else { fail("Secure Enclave unavailable") }
        do {
            let key = try SecureEnclave.P256.Signing.PrivateKey()
            let handle = key.dataRepresentation.base64EncodedString()
            let publicKey = key.publicKey.x963Representation.base64EncodedString()
            print(#"{"handle":"\#(handle)","public_key":"\#(publicKey)"}"#)
        } catch {
            fail("\(error)")
        }
    }

    static func serve() {
        guard let line = readLine(), let handle = Data(base64Encoded: line),
              let key = try? SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: handle)
        else { fail("invalid key handle") }
        while let request = readLine() {
            guard let message = Data(base64Encoded: request), let signature = try? key.signature(for: message) else {
                print("error")
                continue
            }
            print(signature.rawRepresentation.base64EncodedString())
        }
    }

    static func fail(_ message: String) -> Never {
        FileHandle.standardError.write(Data((message + "\n").utf8))
        exit(1)
    }
}
