# Synthetic Matter fixtures

The certificate and PKCS#8 files in this directory are generated test identities.
They do not belong to a home, account or device. Never use them operationally.

The fixtures were produced during the port by the original Go implementation and
are kept as independent reference material:

- `root.der`, `node.der`, `root.tlv`, `node.tlv`, `*-test.pkcs8`: a fabric root
  and node certificate in DER and compact Matter TLV, with their private keys.
  The Rust tests verify signatures over reconstructed DER, compare compact TLV
  byte for byte, and verify private/public key consistency on import.
- `report.bin`, `invoke.bin`: independent Interaction Model encodings.
- `mdns.bin`: an independently compressed DNS-SD packet, carrying a binary Thread
  extended PAN ID so TXT values are not forced through UTF-8.
- `pai.der`, `dac.der`, `attestation.*`, `csr.*`: a synthetic PAI, DAC, signed
  nonce/session elements and a self-signed PKCS#10 CSR, used only to test
  validation and tampering.

The examples next to the plugin source (`spake_check`, `case_check`,
`certificate_check`, `commission_check`, `model_check`, `app_check`) are the Rust
halves of the former interop checks; they can be pointed at any independent peer.
