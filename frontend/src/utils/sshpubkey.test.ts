import { describe, expect, it } from "vitest";
import { NotAnOpenSshKeyError, headerComment, openSshBlob, pubKeyFilename, toOpenSSH, toRFC4716 } from "./sshpubkey";

// Real keys, with the RFC 4716 form produced by `ssh-keygen -e -m RFC4716`. The
// binding property is that our body — the base64 payload — matches ssh-keygen
// byte-for-byte; only the Comment header differs, because we name the key by its
// Cronomicon label instead of ssh-keygen's "converted by <user>@<host>" text.
const ED25519 =
  "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHNkHFq+5PglXPMnY8R7YmcEdV+cMjshwknXLaeT4rzE amadeus-test";
const ED25519_BODY = ["AAAAC3NzaC1lZDI1NTE5AAAAIHNkHFq+5PglXPMnY8R7YmcEdV+cMjshwknXLaeT4rzE"];

const RSA =
  "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCuYktu2rGS9DmmvZivHZtUAL6a52EBn8krcPatSTEYFEngt4IknwefugYIMjtJW1a3g7v/rvrpxPPp4UbrvUdMa/1mLVjxOIIHnm60anK255A3VSEs5wdu5ONhCsAyI0RTY7phxkiUEy/eU1gv54H9p4ahHeDNmJYzmvWcDSUVDrDrwlfDoVKhJpwTYUlXMgsYAqVKtx+PcV3+FbXKleha/YxkxhO9emkN9CEemg+024yFM1LaTVBCcv21pUgms8gcX2cFOAn2HN4/w7uNJ0BbKFjpF0XWJ70fvytjgaK62unHyD8VSczgZHn70EehGWYI/McvwtozwTfmgCsaBrh3 amadeus-test";
const RSA_BODY = [
  "AAAAB3NzaC1yc2EAAAADAQABAAABAQCuYktu2rGS9DmmvZivHZtUAL6a52EBn8krcPatST",
  "EYFEngt4IknwefugYIMjtJW1a3g7v/rvrpxPPp4UbrvUdMa/1mLVjxOIIHnm60anK255A3",
  "VSEs5wdu5ONhCsAyI0RTY7phxkiUEy/eU1gv54H9p4ahHeDNmJYzmvWcDSUVDrDrwlfDoV",
  "KhJpwTYUlXMgsYAqVKtx+PcV3+FbXKleha/YxkxhO9emkN9CEemg+024yFM1LaTVBCcv21",
  "pUgms8gcX2cFOAn2HN4/w7uNJ0BbKFjpF0XWJ70fvytjgaK62unHyD8VSczgZHn70EehGW",
  "YI/McvwtozwTfmgCsaBrh3",
];

/** The base64 payload lines, i.e. everything that is not a marker or a header. */
function bodyOf(rfc: string): string[] {
  return rfc
    .split("\n")
    .filter((l) => l && !l.startsWith("----") && !/^[A-Za-z-]+: /.test(l));
}

describe("toRFC4716", () => {
  it("matches ssh-keygen's body for a single-line ed25519 key", () => {
    expect(bodyOf(toRFC4716(ED25519, "deploy-key"))).toEqual(ED25519_BODY);
  });

  it("matches ssh-keygen's body for a wrapped multi-line RSA key", () => {
    expect(bodyOf(toRFC4716(RSA, "deploy-key"))).toEqual(RSA_BODY);
  });

  it("wraps the body at 70 characters", () => {
    const body = bodyOf(toRFC4716(RSA, "k"));
    expect(body.slice(0, -1).every((l) => l.length === 70)).toBe(true);
    expect(body[body.length - 1].length).toBeLessThanOrEqual(70);
  });

  it("emits the RFC 4716 markers and a trailing newline", () => {
    const out = toRFC4716(ED25519, "deploy-key");
    expect(out.startsWith("---- BEGIN SSH2 PUBLIC KEY ----\n")).toBe(true);
    expect(out.endsWith("---- END SSH2 PUBLIC KEY ----\n")).toBe(true);
  });

  it("names the key by its Cronomicon label", () => {
    expect(toRFC4716(ED25519, "deploy-key")).toContain('Comment: "deploy-key"');
  });

  it("omits the Comment header entirely when there is no label", () => {
    expect(toRFC4716(ED25519)).not.toContain("Comment:");
  });

  it("ignores the OpenSSH trailing comment rather than carrying it over", () => {
    // The source line ends in "amadeus-test"; the header must come from the label.
    expect(toRFC4716(ED25519, "deploy-key")).not.toContain("amadeus-test");
  });

  it("refuses material that is not an OpenSSH public line", () => {
    expect(() => toRFC4716("ssh-ed25519")).toThrow(NotAnOpenSshKeyError);
    expect(() => toRFC4716("")).toThrow(NotAnOpenSshKeyError);
    expect(() => toRFC4716("ssh-ed25519 not*base64!")).toThrow(NotAnOpenSshKeyError);
  });

  it("refuses private-key material, whose second field is base64-shaped", () => {
    // "-----BEGIN OPENSSH PRIVATE KEY-----" splits to a second field of
    // "OPENSSH", which passes a base64 charset check — only the wire-format
    // algorithm-name check rejects it.
    expect(() => toRFC4716("-----BEGIN OPENSSH PRIVATE KEY-----")).toThrow(NotAnOpenSshKeyError);
  });

  it("refuses a well-formed blob whose algorithm name contradicts the type field", () => {
    expect(() => toRFC4716("ssh-rsa " + ED25519_BODY[0])).toThrow(NotAnOpenSshKeyError);
  });
});

describe("openSshBlob", () => {
  it("takes the base64 field, not the type or comment", () => {
    expect(openSshBlob(ED25519)).toBe(ED25519_BODY[0]);
  });

  it("tolerates a key with no trailing comment", () => {
    expect(openSshBlob("ssh-ed25519 " + ED25519_BODY[0])).toBe(ED25519_BODY[0]);
  });
});

describe("headerComment", () => {
  it("escapes backslashes and double quotes (§3.3.2)", () => {
    expect(headerComment('a"b\\c')).toBe('Comment: "a\\"b\\\\c"');
  });

  it("keeps the header within the 72-character ceiling", () => {
    // Long enough that escaping alone would push it over, so truncation must
    // account for the escaped length rather than the raw one.
    const line = headerComment('"'.repeat(80))!;
    expect(line.length).toBeLessThanOrEqual(72);
  });

  it("never truncates in the middle of an escape pair", () => {
    const line = headerComment("\\".repeat(80))!;
    const inner = line.slice('Comment: "'.length, -1);
    // Every backslash must be part of a complete "\\" pair.
    expect(inner.length % 2).toBe(0);
    expect(line.length).toBeLessThanOrEqual(72);
  });

  it("collapses newlines so a comment cannot forge a second header line", () => {
    expect(headerComment("evil\nComment: spoofed")).toBe('Comment: "evil Comment: spoofed"');
  });

  it("returns null for an empty or whitespace-only comment", () => {
    expect(headerComment("")).toBeNull();
    expect(headerComment("   ")).toBeNull();
  });
});

describe("toOpenSSH", () => {
  it("guarantees exactly one trailing newline", () => {
    expect(toOpenSSH(ED25519)).toBe(ED25519 + "\n");
    expect(toOpenSSH(ED25519 + "\n\n")).toBe(ED25519 + "\n");
  });
});

describe("pubKeyFilename", () => {
  it("distinguishes the two formats", () => {
    expect(pubKeyFilename("deploy-key", "openssh")).toBe("deploy-key.pub");
    expect(pubKeyFilename("deploy-key", "rfc4716")).toBe("deploy-key-rfc4716.pub");
  });

  it("folds path separators and spaces out of the filename", () => {
    expect(pubKeyFilename("../../etc/passwd", "openssh")).toBe("etc_passwd.pub");
    expect(pubKeyFilename("my key", "openssh")).toBe("my_key.pub");
  });

  it("falls back rather than producing an empty name or a dotfile", () => {
    expect(pubKeyFilename("", "openssh")).toBe("ssh-key.pub");
    expect(pubKeyFilename("...", "openssh")).toBe("ssh-key.pub");
  });
});
