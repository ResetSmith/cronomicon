// Public-key format conversion for the SSH Keys tab.
//
// An OpenSSH authorized_keys line and an RFC 4716 ("SSH2") public key carry the
// SAME key blob — the base64 field of the OpenSSH line IS the RFC 4716 body. So
// converting between them is text reshaping, not cryptography: no parsing of the
// key blob, no private material, nothing the server has to be asked for.

/** Body lines are wrapped at 70 chars — what `ssh-keygen -e -m RFC4716` emits,
 *  and comfortably inside RFC 4716's 72-char line ceiling. */
const BODY_WRAP = 70;

/** Header lines have the same 72-char ceiling; past it RFC 4716 requires
 *  backslash continuation, which we avoid entirely by truncating the comment. */
const HEADER_MAX = 72;

const BEGIN = "---- BEGIN SSH2 PUBLIC KEY ----";
const END = "---- END SSH2 PUBLIC KEY ----";

/** Thrown for material that isn't a usable OpenSSH public line. */
export class NotAnOpenSshKeyError extends Error {}

/**
 * wireKeyType reads the algorithm name out of a decoded SSH public key blob.
 * The blob's wire format opens with a 4-byte big-endian length followed by that
 * many bytes of algorithm name ("ssh-ed25519", "ssh-rsa", …). Returns null for
 * anything that doesn't decode or doesn't carry a plausible name.
 */
function wireKeyType(blob: string): string | null {
  let bytes: Uint8Array;
  try {
    const bin = atob(blob);
    bytes = Uint8Array.from(bin, (ch) => ch.charCodeAt(0));
  } catch {
    return null;
  }
  if (bytes.length < 4) return null;
  const len = (bytes[0] << 24) | (bytes[1] << 16) | (bytes[2] << 8) | bytes[3];
  if (len <= 0 || len > 64 || bytes.length < 4 + len) return null;
  return new TextDecoder().decode(bytes.subarray(4, 4 + len));
}

/**
 * openSshBlob extracts the base64 key blob from an OpenSSH public line
 * ("<type> <base64> [comment]"), throwing NotAnOpenSshKeyError for anything else.
 *
 * The check goes past "field 2 looks like base64", because that alone accepts
 * text it shouldn't: "-----BEGIN OPENSSH PRIVATE KEY-----" splits into fields
 * whose second is the base64-shaped word "OPENSSH". So we decode the blob and
 * require the algorithm name embedded in the wire format to match the line's own
 * type field — an invariant only a real public key satisfies. Refusing beats
 * emitting a .pub file that no SSH implementation will read.
 */
export function openSshBlob(line: string): string {
  const fields = line.trim().split(/\s+/);
  if (fields.length < 2 || !fields[1]) {
    throw new NotAnOpenSshKeyError("not an OpenSSH public key line");
  }
  const blob = fields[1];
  if (!/^[A-Za-z0-9+/]+={0,2}$/.test(blob)) {
    throw new NotAnOpenSshKeyError("OpenSSH key blob is not base64");
  }
  if (wireKeyType(blob) !== fields[0]) {
    throw new NotAnOpenSshKeyError("OpenSSH key blob does not carry a matching algorithm name");
  }
  return blob;
}

/**
 * headerComment renders an RFC 4716 `Comment:` header, escaping backslashes and
 * double quotes (§3.3.2) and truncating so the whole line stays within the
 * 72-char ceiling — that keeps us clear of the continuation rule. Returns null
 * for an empty comment; the header is optional, so omitting it beats emitting
 * an empty one.
 */
export function headerComment(comment: string): string | null {
  const clean = comment.replace(/[\r\n]+/g, " ").trim();
  if (!clean) return null;
  const escape = (s: string) => s.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  // Truncate against the ESCAPED length (escaping can double a character), and
  // shrink the raw source so a truncation can never split an escape pair.
  let raw = clean;
  const overhead = 'Comment: ""'.length;
  while (raw && overhead + escape(raw).length > HEADER_MAX) {
    raw = raw.slice(0, -1);
  }
  if (!raw) return null;
  return `Comment: "${escape(raw)}"`;
}

/**
 * toRFC4716 converts an OpenSSH public line into RFC 4716 form. `comment`
 * becomes the Comment header (the credential's label is the useful choice —
 * it's what identifies the key in Cronomicon).
 */
export function toRFC4716(openSshLine: string, comment = ""): string {
  const blob = openSshBlob(openSshLine);
  const lines = [BEGIN];
  const header = headerComment(comment);
  if (header) lines.push(header);
  for (let i = 0; i < blob.length; i += BODY_WRAP) {
    lines.push(blob.slice(i, i + BODY_WRAP));
  }
  lines.push(END);
  return lines.join("\n") + "\n";
}

/**
 * toOpenSSH normalizes the stored authorized_keys line for download. The stored
 * value is already trimmed, so this only guarantees the trailing newline that
 * line-oriented files (authorized_keys) expect.
 */
export function toOpenSSH(openSshLine: string): string {
  return openSshLine.trim() + "\n";
}

/**
 * pubKeyFilename builds a download filename from a credential label. Labels are
 * namespace-validated, but this is a filesystem name, so anything outside a
 * conservative set is folded to "_" and an empty result falls back rather than
 * producing a dotfile or an empty name.
 */
export function pubKeyFilename(label: string, format: "openssh" | "rfc4716"): string {
  const safe = label.replace(/[^A-Za-z0-9._-]+/g, "_").replace(/^[._]+/, "").slice(0, 64) || "ssh-key";
  return format === "rfc4716" ? `${safe}-rfc4716.pub` : `${safe}.pub`;
}
