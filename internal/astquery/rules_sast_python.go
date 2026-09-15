package astquery

import (
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Bandit-parity Python SAST ruleset. Patterns target the tree-sitter
// Python grammar; capture names follow the convention
//
//   @match  — the row's anchor (the call / statement to highlight)
//   @fn     — the called function (for `eval(...)`-shaped patterns)
//   @mod    — the module identifier in dotted-attr calls
//   @attr   — the attribute identifier in dotted-attr calls
//   @kw     — keyword argument name
//   @val    — keyword / positional argument value
//   @name   — assignment LHS name / imported module name
//
// Rules are grouped by vulnerability family so adding a rule to a
// family (new pickle helper, another deserialiser) is a one-line
// change.

func init() {
	registerPythonInjection()
	registerPythonDeserialisation()
	registerPythonSubprocess()
	registerPythonCryptoWeakHash()
	registerPythonCryptoWeakCipher()
	registerPythonCryptoModeECB()
	registerPythonNetwork()
	registerPythonSSLTLS()
	registerPythonXML()
	registerPythonDjango()
	registerPythonFlask()
	registerPythonJinja()
	registerPythonSQLi()
	registerPythonHardcoded()
	registerPythonRandom()
	registerPythonFilesystem()
	registerPythonArchive()
	registerPythonImports()
	registerPythonLogging()
	registerPythonRequests()
	registerPythonParamiko()
	registerPythonExceptionHandling()
}

// ---------------------------------------------------------------------------
// 1. Injection — eval / exec / compile  (CWE-95)
// ---------------------------------------------------------------------------

func registerPythonInjection() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-eval-use",
			Description: "Python `eval()` invocation — executes an arbitrary expression. Any caller-controlled input becomes code execution. Use `ast.literal_eval` for safe parsing.",
			Severity:    "error",
			CWE:         "CWE-95",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"injection", "code-injection"},
			References:  []string{"bandit:B307"},
			Pat: map[string]string{
				"python": `((call function: (identifier) @fn) @match (#eq? @fn "eval"))`,
			},
		},
		sastRule{
			Name:        "py-exec-use",
			Description: "Python `exec()` invocation — runs an arbitrary statement string. Caller-controlled input is full RCE.",
			Severity:    "error",
			CWE:         "CWE-95",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"injection", "code-injection"},
			References:  []string{"bandit:B102"},
			Pat: map[string]string{
				"python": `((call function: (identifier) @fn) @match (#eq? @fn "exec"))`,
			},
		},
		sastRule{
			Name:        "py-compile-use",
			Description: "Python `compile()` invocation — produces a code object from a string. Useful for ahead-of-time, but a frequent precursor to `eval` / `exec` of user input.",
			Severity:    "warning",
			CWE:         "CWE-95",
			Tags:        []string{"injection", "code-injection"},
			Pat: map[string]string{
				"python": `((call function: (identifier) @fn) @match (#eq? @fn "compile"))`,
			},
		},
		sastRule{
			Name:        "py-input-use-py2",
			Description: "Python 2 `input()` evaluates its argument as a Python expression (equivalent to `eval(raw_input())`). On Python 3 this is harmless, but a codebase that targets both runtimes leaks RCE in the older path.",
			Severity:    "warning",
			CWE:         "CWE-20",
			Tags:        []string{"injection", "python2"},
			References:  []string{"bandit:B322"},
			Pat: map[string]string{
				"python": `((call function: (identifier) @fn (#eq? @fn "input")) @match)`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 2. Deserialisation — pickle, marshal, yaml.load, shelve  (CWE-502)
// ---------------------------------------------------------------------------

func registerPythonDeserialisation() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-pickle-load",
			Description: "`pickle.load` / `pickle.loads` / `cPickle.load` / `cPickle.loads` deserialise arbitrary Python objects — anything attacker-controlled is RCE. Use `json` or a typed schema (msgpack, protobuf).",
			Severity:    "error",
			CWE:         "CWE-502",
			OWASP:       "A08:2021-Software and Data Integrity Failures",
			Tags:        []string{"deserialization", "rce"},
			References:  []string{"bandit:B301", "bandit:B403"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr) @target) @match
                              (#match? @mod "^(pickle|cPickle|_pickle|dill)$")
                              (#match? @attr "^(load|loads|Unpickler)$"))`,
			},
		},
		sastRule{
			Name:        "py-marshal-load",
			Description: "`marshal.load` / `marshal.loads` — Python's internal serialisation; arbitrary attacker bytes execute on load. Reserve for pyc files the runtime itself emits.",
			Severity:    "error",
			CWE:         "CWE-502",
			Tags:        []string{"deserialization", "rce"},
			References:  []string{"bandit:B302"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "marshal") (#match? @attr "^(load|loads)$"))`,
			},
		},
		sastRule{
			Name:        "py-yaml-load-unsafe",
			Description: "`yaml.load` without an explicit `Loader=yaml.SafeLoader` parses arbitrary YAML tags — `!!python/object/apply` is RCE. Use `yaml.safe_load` or pass `Loader=yaml.SafeLoader`.",
			Severity:    "error",
			CWE:         "CWE-502",
			OWASP:       "A08:2021-Software and Data Integrity Failures",
			Tags:        []string{"deserialization", "yaml"},
			References:  []string{"bandit:B506"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "yaml") (#eq? @attr "load"))`,
			},
		},
		sastRule{
			Name:        "py-shelve-open",
			Description: "`shelve.open(...)` deserialises pickle under the hood; opening an attacker-controlled file means RCE.",
			Severity:    "warning",
			CWE:         "CWE-502",
			Tags:        []string{"deserialization"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "shelve") (#eq? @attr "open"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 3. Subprocess / shell  (CWE-78)
// ---------------------------------------------------------------------------

func registerPythonSubprocess() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-subprocess-shell-true",
			Description: "`subprocess.Popen|call|run|check_output|check_call(..., shell=True)` — runs the command line through `/bin/sh -c`. Any caller-controlled argument is command injection. Pass an argv list instead.",
			Severity:    "error",
			CWE:         "CWE-78",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"injection", "command-injection", "subprocess"},
			References:  []string{"bandit:B602", "bandit:B604"},
			Pat: map[string]string{
				"python": `((call
                                function: (attribute object: (identifier) @mod attribute: (identifier) @attr)
                                arguments: (argument_list
                                    (keyword_argument name: (identifier) @kw value: (true)))) @match
                              (#eq? @mod "subprocess")
                              (#match? @attr "^(Popen|call|run|check_output|check_call)$")
                              (#eq? @kw "shell"))`,
			},
		},
		sastRule{
			Name:        "py-os-system",
			Description: "`os.system(...)` runs the command via `/bin/sh -c`. Any non-literal argument is command injection — use `subprocess.run([..], shell=False)`.",
			Severity:    "error",
			CWE:         "CWE-78",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"injection", "command-injection"},
			References:  []string{"bandit:B605"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "os") (#eq? @attr "system"))`,
			},
		},
		sastRule{
			Name:        "py-os-popen",
			Description: "`os.popen(...)` is a thin shim over `subprocess.Popen(..., shell=True)` and inherits the same injection surface. Deprecated since 2.6.",
			Severity:    "error",
			CWE:         "CWE-78",
			Tags:        []string{"injection", "command-injection", "deprecated"},
			References:  []string{"bandit:B605"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "os") (#match? @attr "^(popen|popen2|popen3|popen4)$"))`,
			},
		},
		sastRule{
			Name:        "py-os-spawn",
			Description: "`os.spawnl` / `os.spawnv` / `os.spawnle` / `os.spawnve` / `os.exec*` family — superseded by `subprocess` for a reason; the shell variants (`spawnlpe`, etc.) inherit shell quoting issues.",
			Severity:    "warning",
			CWE:         "CWE-78",
			Tags:        []string{"command-injection", "deprecated"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "os") (#match? @attr "^(spawnl|spawnle|spawnlp|spawnlpe|spawnv|spawnve|spawnvp|spawnvpe|execl|execle|execlp|execlpe|execv|execve|execvp|execvpe)$"))`,
			},
		},
		sastRule{
			Name:        "py-commands-getoutput",
			Description: "`commands.getoutput` / `commands.getstatusoutput` — Python 2 stdlib, shells out via `/bin/sh -c`. Removed in Python 3; flag any lingering imports.",
			Severity:    "warning",
			CWE:         "CWE-78",
			Tags:        []string{"command-injection", "deprecated"},
			References:  []string{"bandit:B404"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "commands") (#match? @attr "^(getoutput|getstatusoutput)$"))`,
			},
		},
		sastRule{
			Name:        "py-popen2",
			Description: "`popen2.popen2` / `popen2.popen3` / `popen2.Popen3` / `popen2.Popen4` — Python 2 stdlib, deprecated. All variants shell-quote arguments themselves and are routinely misused.",
			Severity:    "warning",
			CWE:         "CWE-78",
			Tags:        []string{"command-injection", "deprecated"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "popen2"))`,
			},
		},
		sastRule{
			Name:        "py-shell-true-import",
			Description: "`from os import system` or `from subprocess import Popen` — the rename hides the dangerous call from downstream `os.system` greps. Audit every use site.",
			Severity:    "info",
			CWE:         "CWE-78",
			Tags:        []string{"command-injection", "import-alias"},
			Pat: map[string]string{
				"python": `((import_from_statement
                                module_name: (dotted_name (identifier) @mod)) @match
                              (#match? @mod "^(os|subprocess|commands|popen2)$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 4. Crypto — weak hash  (CWE-327)
// ---------------------------------------------------------------------------

func registerPythonCryptoWeakHash() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-hashlib-weak-direct",
			Description: "`hashlib.md5(...)` / `hashlib.sha1(...)` — cryptographically broken for any security-sensitive purpose. Use `hashlib.sha256` (or BLAKE2 / SHA-3) for fingerprinting and `argon2` / `bcrypt` / `scrypt` / `pbkdf2_hmac` for password hashing.",
			Severity:    "error",
			CWE:         "CWE-327",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"crypto", "weak-hash"},
			References:  []string{"bandit:B303", "bandit:B324"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "hashlib") (#match? @attr "^(md5|sha1|md4)$"))`,
			},
		},
		sastRule{
			Name:        "py-hashlib-new-weak",
			Description: "`hashlib.new(\"md5\"|\"sha1\"|\"md4\"|\"ripemd160\")` — same weak-hash story as direct ctor, hidden behind the `new()` factory.",
			Severity:    "error",
			CWE:         "CWE-327",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"crypto", "weak-hash"},
			References:  []string{"bandit:B324"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)
                                arguments: (argument_list (string) @algo)) @match
                              (#eq? @mod "hashlib") (#eq? @attr "new")
                              (#match? @algo "[\"']?(md5|sha1|md4|ripemd160)[\"']?"))`,
			},
		},
		sastRule{
			Name:        "py-pycryptodome-weak-hash",
			Description: "`from Crypto.Hash import MD5|SHA1|MD4|RIPEMD` (or pycryptodome's `Cryptodome.Hash` mirror) — pycryptodome wrappers for cryptographically-broken hashes.",
			Severity:    "error",
			CWE:         "CWE-327",
			Tags:        []string{"crypto", "weak-hash"},
			Pat: map[string]string{
				"python": `((import_from_statement
                                module_name: (dotted_name) @mod
                                name: (dotted_name (identifier) @name)) @match
                              (#match? @mod "^(Crypto|Cryptodome)\\.Hash$")
                              (#match? @name "^(MD5|MD4|SHA1|RIPEMD)$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 5. Crypto — weak cipher  (CWE-327)
// ---------------------------------------------------------------------------

func registerPythonCryptoWeakCipher() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-weak-cipher-pycrypto",
			Description: "`from Crypto.Cipher import DES|ARC2|ARC4|Blowfish` (or pycryptodome's `Cryptodome.Cipher` mirror) — DES / RC2 / RC4 / 64-bit Blowfish are all broken. Use AES-GCM or ChaCha20-Poly1305.",
			Severity:    "error",
			CWE:         "CWE-327",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"crypto", "weak-cipher"},
			References:  []string{"bandit:B304"},
			Pat: map[string]string{
				"python": `((import_from_statement
                                module_name: (dotted_name) @mod
                                name: (dotted_name (identifier) @name)) @match
                              (#match? @mod "^(Crypto|Cryptodome)\\.Cipher$")
                              (#match? @name "^(DES|DES3|ARC2|ARC4|XOR|Blowfish)$"))`,
			},
		},
		sastRule{
			Name:        "py-cryptography-weak-cipher",
			Description: "`cryptography.hazmat.primitives.ciphers.algorithms.TripleDES|Blowfish|ARC4|IDEA|CAST5|SEED` — legacy ciphers retained for interop only; new code should use AES-GCM or ChaCha20-Poly1305.",
			Severity:    "warning",
			CWE:         "CWE-327",
			Tags:        []string{"crypto", "weak-cipher"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @algo)) @match
                              (#match? @algo "^(TripleDES|Blowfish|ARC4|IDEA|CAST5|SEED)$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 6. Crypto — ECB mode  (CWE-327)
// ---------------------------------------------------------------------------

func registerPythonCryptoModeECB() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-aes-mode-ecb",
			Description: "AES in ECB mode (`AES.MODE_ECB` / `modes.ECB()`) — does not hide patterns in the plaintext; identical blocks produce identical ciphertext. Use AES-GCM or AES-CBC with a random IV + MAC.",
			Severity:    "error",
			CWE:         "CWE-327",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"crypto", "ecb"},
			References:  []string{"bandit:B305"},
			Pat: map[string]string{
				"python": `((attribute object: (identifier) @mod attribute: (identifier) @attr) @match
                              (#match? @attr "^(MODE_ECB|ECB)$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 7. Network — cleartext protocols  (CWE-319)
// ---------------------------------------------------------------------------

func registerPythonNetwork() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-telnetlib-import",
			Description: "`import telnetlib` — Telnet transmits credentials in cleartext. Use SSH (paramiko / asyncssh) or HTTPS. Removed from stdlib in Python 3.13.",
			Severity:    "error",
			CWE:         "CWE-319",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"cleartext", "deprecated"},
			References:  []string{"bandit:B401"},
			Pat: map[string]string{
				"python": `((import_statement name: (dotted_name (identifier) @name)) @match (#eq? @name "telnetlib"))`,
			},
		},
		sastRule{
			Name:        "py-ftplib-import",
			Description: "`import ftplib` — FTP transmits credentials in cleartext. Use SFTP (paramiko) or FTPS (`ftplib.FTP_TLS`).",
			Severity:    "warning",
			CWE:         "CWE-319",
			Tags:        []string{"cleartext"},
			References:  []string{"bandit:B402"},
			Pat: map[string]string{
				"python": `((import_statement name: (dotted_name (identifier) @name)) @match (#eq? @name "ftplib"))`,
			},
		},
		sastRule{
			Name:        "py-nntplib-import",
			Description: "`import nntplib` — NNTP transmits credentials in cleartext; deprecated in Python 3.11, removed in 3.13.",
			Severity:    "warning",
			CWE:         "CWE-319",
			Tags:        []string{"cleartext", "deprecated"},
			Pat: map[string]string{
				"python": `((import_statement name: (dotted_name (identifier) @name)) @match (#eq? @name "nntplib"))`,
			},
		},
		sastRule{
			Name:        "py-imaplib-no-starttls",
			Description: "`imaplib.IMAP4(...)` (not `IMAP4_SSL`) connects without TLS. Use `IMAP4_SSL` or call `IMAP4.starttls()` immediately after connect.",
			Severity:    "warning",
			CWE:         "CWE-319",
			Tags:        []string{"cleartext"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "imaplib") (#eq? @attr "IMAP4"))`,
			},
		},
		sastRule{
			Name:        "py-poplib-no-tls",
			Description: "`poplib.POP3(...)` (not `POP3_SSL`) connects without TLS. POP3 transmits credentials in cleartext otherwise.",
			Severity:    "warning",
			CWE:         "CWE-319",
			Tags:        []string{"cleartext"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "poplib") (#eq? @attr "POP3"))`,
			},
		},
		sastRule{
			Name:        "py-smtplib-no-starttls",
			Description: "`smtplib.SMTP(...)` without a subsequent `starttls()` call leaks credentials in cleartext. Use `SMTP_SSL` or call `starttls()` immediately.",
			Severity:    "info",
			CWE:         "CWE-319",
			Tags:        []string{"cleartext"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "smtplib") (#eq? @attr "SMTP"))`,
			},
		},
		sastRule{
			Name:        "py-socket-bind-all-interfaces",
			Description: "Socket bound to `0.0.0.0` / empty string — exposes the service on every interface, often unintentional in development paths shipped to prod. Bind to `127.0.0.1` unless the service is genuinely public.",
			Severity:    "warning",
			CWE:         "CWE-605",
			Tags:        []string{"bind", "all-interfaces"},
			References:  []string{"bandit:B104"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @attr)
                                arguments: (argument_list (tuple (string) @host))) @match
                              (#eq? @attr "bind")
                              (#match? @host "^[\"']?(0\\.0\\.0\\.0|::|\\s*)[\"']?$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 8. SSL / TLS  (CWE-295, CWE-326)
// ---------------------------------------------------------------------------

func registerPythonSSLTLS() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-ssl-unverified-context",
			Description: "`ssl._create_unverified_context()` — bypasses certificate validation. Lets any MITM impersonate the server.",
			Severity:    "error",
			CWE:         "CWE-295",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"tls", "no-verify"},
			References:  []string{"bandit:B501"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "ssl") (#eq? @attr "_create_unverified_context"))`,
			},
		},
		sastRule{
			Name:        "py-ssl-no-verify",
			Description: "`ssl.CERT_NONE` selected as `verify_mode` — disables certificate validation. Any MITM with a self-signed cert is accepted.",
			Severity:    "error",
			CWE:         "CWE-295",
			Tags:        []string{"tls", "no-verify"},
			References:  []string{"bandit:B504"},
			Pat: map[string]string{
				"python": `((attribute object: (identifier) @mod attribute: (identifier) @attr) @match
                              (#eq? @mod "ssl") (#eq? @attr "CERT_NONE"))`,
			},
		},
		sastRule{
			Name:        "py-ssl-old-protocol",
			Description: "`ssl.PROTOCOL_SSLv2|SSLv3|TLSv1|TLSv1_1` — broken or deprecated TLS versions. Use `PROTOCOL_TLS_CLIENT` / `PROTOCOL_TLS_SERVER` which negotiate the highest mutually-supported version.",
			Severity:    "error",
			CWE:         "CWE-326",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"tls", "weak-protocol"},
			References:  []string{"bandit:B502", "bandit:B503"},
			Pat: map[string]string{
				"python": `((attribute object: (identifier) @mod attribute: (identifier) @attr) @match
                              (#eq? @mod "ssl") (#match? @attr "^(PROTOCOL_SSLv2|PROTOCOL_SSLv3|PROTOCOL_TLSv1|PROTOCOL_TLSv1_1)$"))`,
			},
		},
		sastRule{
			Name:        "py-paramiko-autoadd-policy",
			Description: "`paramiko.AutoAddPolicy()` — accepts any host key automatically, defeating SSH host-key verification. Use `paramiko.RejectPolicy()` with a known_hosts file.",
			Severity:    "error",
			CWE:         "CWE-295",
			Tags:        []string{"ssh", "host-key"},
			References:  []string{"bandit:B507"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "paramiko") (#match? @attr "^(AutoAddPolicy|MissingHostKeyPolicy|WarningPolicy)$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 9. XML — XXE / billion-laughs / external-entity expansion  (CWE-611)
// ---------------------------------------------------------------------------

func registerPythonXML() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-xml-etree-no-defusedxml",
			Description: "`from xml.etree import …` / `import xml.etree.…` — stdlib XML parser is vulnerable to XXE + billion-laughs. Use `defusedxml.ElementTree` instead. Rule fires on the import; review the call site.",
			Severity:    "error",
			CWE:         "CWE-611",
			OWASP:       "A05:2021-Security Misconfiguration",
			Tags:        []string{"xxe", "xml"},
			References:  []string{"bandit:B313", "bandit:B314"},
			Pat: map[string]string{
				"python": `((import_from_statement module_name: (dotted_name) @mod) @match
				                 (#match? @mod "^xml\\.etree(\\.|$)"))
				                ((import_statement name: (dotted_name) @mod) @match
				                 (#match? @mod "^xml\\.etree(\\.|$)"))`,
			},
		},
		sastRule{
			Name:        "py-xml-sax-no-defusedxml",
			Description: "`xml.sax.*` import — XXE-vulnerable stdlib parser. Use `defusedxml.sax`.",
			Severity:    "error",
			CWE:         "CWE-611",
			Tags:        []string{"xxe", "xml"},
			References:  []string{"bandit:B317"},
			Pat: map[string]string{
				"python": `((import_from_statement module_name: (dotted_name) @mod) @match
				                 (#match? @mod "^xml\\.sax(\\.|$)"))
				                ((import_statement name: (dotted_name) @mod) @match
				                 (#match? @mod "^xml\\.sax(\\.|$)"))`,
			},
		},
		sastRule{
			Name:        "py-xml-minidom-no-defusedxml",
			Description: "`xml.dom.minidom` import — XXE-vulnerable stdlib parser. Use `defusedxml.minidom`.",
			Severity:    "error",
			CWE:         "CWE-611",
			Tags:        []string{"xxe", "xml"},
			References:  []string{"bandit:B318"},
			Pat: map[string]string{
				"python": `((import_from_statement module_name: (dotted_name) @mod
				                  name: (dotted_name (identifier) @name)) @match
				                 (#eq? @mod "xml.dom") (#eq? @name "minidom"))
				                ((import_from_statement module_name: (dotted_name) @mod) @match
				                 (#eq? @mod "xml.dom.minidom"))
				                ((import_statement name: (dotted_name) @mod) @match
				                 (#eq? @mod "xml.dom.minidom"))`,
			},
		},
		sastRule{
			Name:        "py-xml-pulldom-no-defusedxml",
			Description: "`xml.dom.pulldom` import — XXE-vulnerable stdlib parser. Use `defusedxml.pulldom`.",
			Severity:    "error",
			CWE:         "CWE-611",
			Tags:        []string{"xxe", "xml"},
			Pat: map[string]string{
				"python": `((import_from_statement module_name: (dotted_name) @mod
				                  name: (dotted_name (identifier) @name)) @match
				                 (#eq? @mod "xml.dom") (#eq? @name "pulldom"))
				                ((import_from_statement module_name: (dotted_name) @mod) @match
				                 (#eq? @mod "xml.dom.pulldom"))
				                ((import_statement name: (dotted_name) @mod) @match
				                 (#eq? @mod "xml.dom.pulldom"))`,
			},
		},
		sastRule{
			Name:        "py-lxml-no-resolve-entities",
			Description: "`lxml` import — XXE-vulnerable unless every parser is built with `resolve_entities=False`. Use `defusedxml.lxml`, or audit every call site manually.",
			Severity:    "warning",
			CWE:         "CWE-611",
			Tags:        []string{"xxe", "xml", "lxml"},
			Pat: map[string]string{
				"python": `((import_from_statement module_name: (dotted_name) @mod) @match
				                 (#match? @mod "^lxml(\\.|$)"))
				                ((import_statement name: (dotted_name) @mod) @match
				                 (#match? @mod "^lxml(\\.|$)"))`,
			},
		},
		sastRule{
			Name:        "py-xmlrpc-import",
			Description: "`import xmlrpc.client` / `import xmlrpc.server` — XML-RPC unmarshals method arguments via the standard XML parser; same XXE risk as raw `xml.etree`. Use `defusedxml.xmlrpc.monkey_patch()` or migrate to JSON-RPC.",
			Severity:    "warning",
			CWE:         "CWE-611",
			Tags:        []string{"xxe", "xml-rpc"},
			References:  []string{"bandit:B411"},
			Pat: map[string]string{
				"python": `((import_statement name: (dotted_name) @name) @match (#match? @name "^xmlrpc(\\.client|\\.server)?$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 10. Django  (CWE-79 / CWE-89 / CWE-489 / CWE-352)
// ---------------------------------------------------------------------------

func registerPythonDjango() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-django-mark-safe",
			Description: "`django.utils.safestring.mark_safe(...)` — flags arbitrary text as safe-from-escaping. Calling with user input is XSS.",
			Severity:    "error",
			CWE:         "CWE-79",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"xss", "django"},
			References:  []string{"bandit:B703"},
			Pat: map[string]string{
				"python": `((call function: [(identifier) @fn (attribute attribute: (identifier) @fn)]) @match (#eq? @fn "mark_safe"))`,
			},
		},
		sastRule{
			Name:        "py-django-extra-method",
			Description: "Django ORM `.extra(...)` clause — bypasses ORM escaping and lets raw SQL fragments through.",
			Severity:    "error",
			CWE:         "CWE-89",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"sqli", "django"},
			References:  []string{"bandit:B610"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @fn)) @match (#eq? @fn "extra"))`,
			},
		},
		sastRule{
			Name:        "py-django-raw-query",
			Description: "Django ORM `.raw(...)` clause — executes raw SQL on the manager; trivially exploitable with user input.",
			Severity:    "error",
			CWE:         "CWE-89",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"sqli", "django"},
			References:  []string{"bandit:B611"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @fn)) @match (#eq? @fn "raw"))`,
			},
		},
		sastRule{
			Name:        "py-django-csrf-exempt",
			Description: "`@csrf_exempt` decorator disables CSRF protection on a view. Use `@csrf_protect` or design the view to be safe-by-default.",
			Severity:    "warning",
			CWE:         "CWE-352",
			OWASP:       "A01:2021-Broken Access Control",
			Tags:        []string{"csrf", "django"},
			Pat: map[string]string{
				"python": `((decorator [(identifier) @fn (attribute attribute: (identifier) @fn) (call function: [(identifier) @fn (attribute attribute: (identifier) @fn)])]) @match (#eq? @fn "csrf_exempt"))`,
			},
		},
		sastRule{
			Name:        "py-django-debug-true",
			Description: "`DEBUG = True` at module scope in settings — leaks stack traces, environment, and SECRET_KEY in error pages.",
			Severity:    "warning",
			CWE:         "CWE-489",
			OWASP:       "A05:2021-Security Misconfiguration",
			Tags:        []string{"django", "debug"},
			Pat: map[string]string{
				"python": `((assignment left: (identifier) @name right: (true)) @match (#eq? @name "DEBUG"))`,
			},
		},
		sastRule{
			Name:        "py-django-allowed-hosts-wildcard",
			Description: "`ALLOWED_HOSTS = ['*']` — disables Django's Host header validation. Allows host header attacks (cache poisoning, password-reset poisoning).",
			Severity:    "warning",
			CWE:         "CWE-942",
			Tags:        []string{"django", "host-header"},
			Pat: map[string]string{
				"python": `((assignment left: (identifier) @name right: (list (string) @host)) @match
                              (#eq? @name "ALLOWED_HOSTS") (#match? @host "[\"']?\\*[\"']?"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 11. Flask  (CWE-489 / CWE-79)
// ---------------------------------------------------------------------------

func registerPythonFlask() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-flask-debug-true",
			Description: "`app.run(debug=True)` — exposes the Werkzeug debugger on the network. Anyone who hits the dev server can run arbitrary Python via the debug console.",
			Severity:    "error",
			CWE:         "CWE-489",
			OWASP:       "A05:2021-Security Misconfiguration",
			Tags:        []string{"flask", "debug"},
			References:  []string{"bandit:B201"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @fn)
                                arguments: (argument_list (keyword_argument name: (identifier) @kw value: (true)))) @match
                              (#eq? @fn "run") (#eq? @kw "debug"))`,
			},
		},
		sastRule{
			Name:        "py-flask-render-template-string",
			Description: "`flask.render_template_string(...)` — Jinja2 templating from a runtime string; if the string contains user input, this is SSTI / RCE.",
			Severity:    "warning",
			CWE:         "CWE-94",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"flask", "ssti"},
			Pat: map[string]string{
				"python": `((call function: [(identifier) @fn (attribute attribute: (identifier) @fn)]) @match (#eq? @fn "render_template_string"))`,
			},
		},
		sastRule{
			Name:        "py-flask-redirect-user-input",
			Description: "`flask.redirect(...)` to a value that flows from `request.*` — open redirect. Validate against an allow-list of paths.",
			Severity:    "info",
			CWE:         "CWE-601",
			Tags:        []string{"flask", "open-redirect"},
			Pat: map[string]string{
				"python": `((call function: [(identifier) @fn (attribute attribute: (identifier) @fn)]
                                arguments: (argument_list (attribute object: (identifier) @src))) @match
                              (#eq? @fn "redirect") (#eq? @src "request"))`,
			},
		},
		sastRule{
			Name:        "py-flask-send-file-user-input",
			Description: "`flask.send_file(path)` where `path` flows from `request.*` — path traversal: an attacker reads any file the process can open. Resolve and validate against a base directory.",
			Severity:    "error",
			CWE:         "CWE-22",
			OWASP:       "A01:2021-Broken Access Control",
			Tags:        []string{"flask", "path-traversal"},
			Pat: map[string]string{
				"python": `((call function: [(identifier) @fn (attribute attribute: (identifier) @fn)]
                                arguments: (argument_list (attribute object: (identifier) @src))) @match
                              (#eq? @fn "send_file") (#eq? @src "request"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 12. Jinja2  (CWE-79)
// ---------------------------------------------------------------------------

func registerPythonJinja() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-jinja2-autoescape-false",
			Description: "`jinja2.Environment(autoescape=False)` — explicitly opts out of HTML escaping. Any user-controlled variable rendered into a template becomes XSS.",
			Severity:    "error",
			CWE:         "CWE-79",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"xss", "jinja2"},
			References:  []string{"bandit:B701"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)
                                arguments: (argument_list (keyword_argument name: (identifier) @kw value: (false)))) @match
                              (#eq? @mod "jinja2") (#eq? @attr "Environment") (#eq? @kw "autoescape"))`,
			},
		},
		sastRule{
			Name:        "py-mako-template-no-default-filters",
			Description: "`mako.template.Template(...)` without `default_filters=['h']` doesn't HTML-escape by default — XSS when used to render web responses.",
			Severity:    "info",
			CWE:         "CWE-79",
			Tags:        []string{"xss", "mako"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "mako") (#eq? @attr "Template"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 13. SQL injection — formatted query strings  (CWE-89)
// ---------------------------------------------------------------------------

func registerPythonSQLi() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-sqli-execute-format",
			Description: "`cursor.execute(\"...{}...\".format(x))` — Python str.format() builds the query before the driver sees it. The driver's placeholder mechanism (`%s` + `params` tuple) is bypassed entirely.",
			Severity:    "error",
			CWE:         "CWE-89",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"sqli", "format"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @fn)
                                arguments: (argument_list
                                    (call function: (attribute attribute: (identifier) @sfn)))) @match
                              (#match? @fn "^(execute|executemany|raw|fetch|fetchall|fetchone)$") (#eq? @sfn "format"))`,
			},
		},
		sastRule{
			Name:        "py-sqli-execute-percent",
			Description: "`cursor.execute(\"... %s ...\" % values)` — Python % formatting builds the query string before parameterisation. Same SQL-injection risk as `.format()`.",
			Severity:    "error",
			CWE:         "CWE-89",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"sqli", "percent-format"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @fn)
                                arguments: (argument_list
                                    (binary_operator operator: "%"))) @match
                              (#match? @fn "^(execute|executemany|raw|fetch|fetchall|fetchone)$"))`,
			},
		},
		sastRule{
			Name:        "py-sqli-execute-fstring",
			Description: "`cursor.execute(f\"... {val} ...\")` — f-string interpolation runs before the driver; user-controlled `val` is SQL-injection.",
			Severity:    "error",
			CWE:         "CWE-89",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"sqli", "fstring"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @fn)
                                arguments: (argument_list (string (interpolation)))) @match
                              (#match? @fn "^(execute|executemany|raw|fetch|fetchall|fetchone)$"))`,
			},
			PostFilter: fstringInterpolatesNonConstant,
		},
		sastRule{
			Name:        "py-sqlalchemy-text-with-fstring",
			Description: "`sqlalchemy.text(f\"...\")` — building the SQL text via f-string interpolation rather than `:bindparam` placeholders is SQL-injection.",
			Severity:    "error",
			CWE:         "CWE-89",
			OWASP:       "A03:2021-Injection",
			Tags:        []string{"sqli", "sqlalchemy"},
			Pat: map[string]string{
				"python": `((call function: [(identifier) @fn (attribute attribute: (identifier) @fn)]
                                arguments: (argument_list (string (interpolation)))) @match
                              (#eq? @fn "text"))`,
			},
			PostFilter: fstringInterpolatesNonConstant,
		},
	)
}

// fstringInterpolatesNonConstant keeps an f-string SQLi match unless every
// `{expr}` in the call's f-string arguments is a constant by PEP 8
// convention. SQL placeholders cannot bind identifiers, so column lists
// and table names are routinely interpolated from module constants:
//
//	COLUMNS = "id, name"
//	conn.execute(f"SELECT {COLUMNS} FROM t WHERE id = ?", (i,))
//
// An expression counts as constant when it is an UPPER_CASE name or an
// attribute ending in one (`schema.COLUMNS`), or a name bound by an
// enclosing `for` over such a constant within the same function:
//
//	for column, sql_type in _ADDED_COLUMNS:
//	    conn.execute(f"ALTER TABLE t ADD COLUMN {column} {sql_type}")
//
// Anything else — a parameter, a call, a subscript, a lower-case name —
// keeps the match. The check is syntactic: a constant reassigned from user
// input is not detected, which is the convention's own failure mode.
func fstringInterpolatesNonConstant(qr parser.QueryResult, src []byte) bool {
	m, ok := qr.Captures["match"]
	if !ok || m.Node == nil {
		return true
	}
	args := m.Node.ChildByFieldName("arguments")
	if args == nil {
		return true
	}
	for arg := range args.NamedChildren() {
		if arg.Type() != "string" {
			continue
		}
		for part := range arg.NamedChildren() {
			if part.Type() != "interpolation" {
				continue
			}
			expr := part.ChildByFieldName("expression")
			if expr == nil || !pyConstantExpr(expr, src, true) {
				return true
			}
		}
	}
	return false
}

// pyConstantExpr reports whether expr is an UPPER_CASE name, an attribute
// ending in one, or (when followLoops is set) a name bound by an enclosing
// `for` loop whose iterable is itself constant.
func pyConstantExpr(expr *sitter.Node, src []byte, followLoops bool) bool {
	switch expr.Type() {
	case "attribute":
		attr := expr.ChildByFieldName("attribute")
		return attr != nil && isPyConstantName(attr.Content(src))
	case "identifier":
		name := expr.Content(src)
		if isPyConstantName(name) {
			return true
		}
		return followLoops && boundByLoopOverConstant(expr, name, src)
	}
	return false
}

// boundByLoopOverConstant walks outwards from n to the nearest enclosing
// scope boundary, looking for a `for` statement whose target binds name.
// The innermost such loop decides: it binds name to an element of its
// iterable, so the name is constant exactly when the iterable is.
func boundByLoopOverConstant(n *sitter.Node, name string, src []byte) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		switch p.Type() {
		case "function_definition", "lambda", "class_definition":
			return false
		case "for_statement":
			if !pyTargetBinds(p.ChildByFieldName("left"), name, src) {
				continue
			}
			right := p.ChildByFieldName("right")
			return right != nil && pyConstantExpr(right, src, false)
		}
	}
	return false
}

// pyTargetBinds reports whether a `for` target (a name, or a possibly nested
// tuple/list pattern) binds name.
func pyTargetBinds(target *sitter.Node, name string, src []byte) bool {
	if target == nil {
		return false
	}
	if target.Type() == "identifier" {
		return target.Content(src) == name
	}
	for child := range target.NamedChildren() {
		if pyTargetBinds(child, name, src) {
			return true
		}
	}
	return false
}

// isPyConstantName matches PEP 8 constant names: optional leading
// underscores, then an upper-case letter and at least one more upper-case
// letter, digit or underscore. A single capital (`Q`) is too ambiguous.
func isPyConstantName(s string) bool {
	i := 0
	for i < len(s) && s[i] == '_' {
		i++
	}
	rest := s[i:]
	if len(rest) < 2 || rest[0] < 'A' || rest[0] > 'Z' {
		return false
	}
	for j := 1; j < len(rest); j++ {
		c := rest[j]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 14. Hardcoded credentials  (CWE-798)
// ---------------------------------------------------------------------------

func registerPythonHardcoded() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-hardcoded-credential-keyword-arg",
			Description: "Function call passes `password=\"…\"` / `secret=\"…\"` / `api_key=\"…\"` etc. with a non-trivial string literal — embeds the credential in source and version control. Move to env / secrets manager.",
			Severity:    "error",
			CWE:         "CWE-798",
			OWASP:       "A07:2021-Identification and Authentication Failures",
			Tags:        []string{"secrets", "credentials"},
			References:  []string{"bandit:B106"},
			Pat: map[string]string{
				"python": `((call arguments: (argument_list
                                (keyword_argument name: (identifier) @name value: (string) @val))) @match
                              (#match? @name "(?i)^(password|passwd|pass|secret|api_?key|token|aws_?secret(_?key)?|access_?key|private_?key)$"))`,
			},
			PostFilter: secretLiteralLooksReal,
		},
		sastRule{
			Name:        "py-hardcoded-credential-default-arg",
			Description: "Function default parameter named like a credential (`password=\"...\"`, `api_key=\"...\"`) with a non-trivial string literal — exposed in `help()` output and source control.",
			Severity:    "error",
			CWE:         "CWE-798",
			Tags:        []string{"secrets", "credentials"},
			References:  []string{"bandit:B107"},
			Pat: map[string]string{
				"python": `((default_parameter name: (identifier) @name value: (string) @val) @match
                              (#match? @name "(?i)^(password|passwd|pass|secret|api_?key|token|aws_?secret(_?key)?|access_?key|private_?key)$"))`,
			},
			PostFilter: secretLiteralLooksReal,
		},
		sastRule{
			Name:        "py-django-secret-key-hardcoded",
			Description: "`SECRET_KEY = \"...\"` literal in Django settings — must be loaded from env / KMS, not committed.",
			Severity:    "error",
			CWE:         "CWE-798",
			Tags:        []string{"secrets", "django"},
			Pat: map[string]string{
				"python": `((assignment left: (identifier) @name right: (string) @val) @match (#eq? @name "SECRET_KEY"))`,
			},
			PostFilter: secretLiteralLooksReal,
		},
	)
}

// secretLiteralLooksReal drops obvious placeholder strings so the
// credential detectors don't spam every example / test fixture.
// Shared by py-hardcoded-credential-* and py-django-secret-key-*.
func secretLiteralLooksReal(qr parser.QueryResult, _ []byte) bool {
	v, ok := qr.Captures["val"]
	if !ok {
		return false
	}
	text := trimQuotes(v.Text)
	if len(text) < 8 {
		return false
	}
	low := lower(text)
	for _, marker := range []string{"todo", "fixme", "changeme", "placeholder", "example", "your-", "xxx", "***", "...", "<", "test", "demo", "dummy", "sample"} {
		if containsStr(low, marker) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 15. Insecure random  (CWE-330)
// ---------------------------------------------------------------------------

func registerPythonRandom() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-random-for-secret",
			Description: "`random.random` / `random.randint` / `random.choice` / `random.choices` etc. — `random` is a Mersenne Twister, not cryptographically secure. Use `secrets` module for any token / id / key.",
			Severity:    "warning",
			CWE:         "CWE-330",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"crypto", "insecure-random"},
			References:  []string{"bandit:B311"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "random") (#match? @attr "^(random|randint|randrange|choice|choices|sample|uniform|getrandbits|shuffle)$"))`,
			},
		},
		sastRule{
			Name:        "py-uuid1-predictable",
			Description: "`uuid.uuid1()` embeds the host MAC address and a timestamp — predictable across hosts. Use `uuid.uuid4()` for unguessable identifiers.",
			Severity:    "info",
			CWE:         "CWE-330",
			Tags:        []string{"random", "uuid"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "uuid") (#eq? @attr "uuid1"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 16. Filesystem — tempfile.mktemp / hardcoded tmp paths / chmod  (CWE-377, CWE-732)
// ---------------------------------------------------------------------------

func registerPythonFilesystem() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-tempfile-mktemp",
			Description: "`tempfile.mktemp()` — race-prone: the returned name can be created by another process between the call and the open. Use `tempfile.mkstemp()` or `tempfile.NamedTemporaryFile()`.",
			Severity:    "error",
			CWE:         "CWE-377",
			Tags:        []string{"tempfile", "race"},
			References:  []string{"bandit:B306"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "tempfile") (#eq? @attr "mktemp"))`,
			},
		},
		sastRule{
			Name:        "py-hardcoded-tmp-path",
			Description: "Hardcoded `/tmp/...` / `/var/tmp/...` path passed to `open(...)` / `os.path.join` etc. — predictable temp paths invite symlink attacks. Use `tempfile.mkstemp()`.",
			Severity:    "warning",
			CWE:         "CWE-377",
			Tags:        []string{"tempfile", "race"},
			References:  []string{"bandit:B108"},
			Pat: map[string]string{
				"python": `((string) @match (#match? @match "^[\"'](/tmp|/var/tmp|/dev/shm)/[^\"']+[\"']$"))`,
			},
		},
		sastRule{
			Name:        "py-chmod-world-writable",
			Description: "`os.chmod(..., 0o666)` / `0o777` / `stat.S_IWOTH` — world-writable file. Almost always a privilege-management bug.",
			Severity:    "warning",
			CWE:         "CWE-732",
			Tags:        []string{"permissions"},
			References:  []string{"bandit:B103"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)
                                arguments: (argument_list (_) (integer) @mode)) @match
                              (#eq? @mod "os") (#eq? @attr "chmod")
                              (#match? @mode "^(0o?7[0-7]7|0o?6[0-7]6)$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 17. Archive extraction — zipslip / tar traversal  (CWE-22)
// ---------------------------------------------------------------------------

func registerPythonArchive() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-tarfile-extractall",
			Description: "`tarfile.extractall(...)` without `filter=` (3.12+) or per-member path validation — \"tar slip\" / path-traversal: a malicious archive writes outside the destination.",
			Severity:    "warning",
			CWE:         "CWE-22",
			Tags:        []string{"path-traversal", "tar-slip"},
			References:  []string{"bandit:B202"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @fn)) @match
                              (#match? @fn "^(extractall|extract)$"))`,
			},
			PostFilter: func(qr parser.QueryResult, _ []byte) bool {
				m, ok := qr.Captures["match"]
				if !ok {
					return true
				}
				// 3.12+'s `filter=` kwarg (filter="data" / "tar"
				// / a custom `TarInfo → TarInfo` callback) makes
				// the call safe. If the caller passed it, drop
				// the match.
				return !containsStr(m.Text, "filter=")
			},
		},
		sastRule{
			Name:        "py-shutil-unpack-archive-user-input",
			Description: "`shutil.unpack_archive(...)` — no per-member validation; identical zipslip / tarslip exposure as raw `tarfile.extractall`.",
			Severity:    "info",
			CWE:         "CWE-22",
			Tags:        []string{"path-traversal"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
                              (#eq? @mod "shutil") (#eq? @attr "unpack_archive"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 18. Insecure imports of removed / weak modules
// ---------------------------------------------------------------------------

func registerPythonImports() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-import-pickle",
			Description: "`import pickle` / `import cPickle` / `import dill` — flag every site so an audit can ensure no `load()` is called on untrusted bytes.",
			Severity:    "info",
			CWE:         "CWE-502",
			Tags:        []string{"deserialization", "audit-hook"},
			References:  []string{"bandit:B403"},
			Pat: map[string]string{
				"python": `((import_statement name: (dotted_name (identifier) @name)) @match (#match? @name "^(pickle|cPickle|dill|_pickle)$"))`,
			},
		},
		sastRule{
			Name:        "py-import-subprocess",
			Description: "`import subprocess` — flag for audit. Most uses are fine, but cohort-wide most command-injection vulns trace back to this module.",
			Severity:    "info",
			CWE:         "CWE-78",
			Tags:        []string{"audit-hook"},
			References:  []string{"bandit:B404"},
			Pat: map[string]string{
				"python": `((import_statement name: (dotted_name (identifier) @name)) @match (#eq? @name "subprocess"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 19. Logging  (CWE-117, CWE-94)
// ---------------------------------------------------------------------------

func registerPythonLogging() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-logging-config-listen",
			Description: "`logging.config.listen()` — accepts a config update on a TCP socket; any caller can re-program the logger to call arbitrary handlers. RCE-class.",
			Severity:    "error",
			CWE:         "CWE-94",
			Tags:        []string{"logging", "remote-config"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (attribute object: (identifier) @mod attribute: (identifier) @sub) attribute: (identifier) @fn)) @match
                              (#eq? @mod "logging") (#eq? @sub "config") (#eq? @fn "listen"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 20. requests / urllib  (CWE-295, CWE-918, CWE-22)
// ---------------------------------------------------------------------------

func registerPythonRequests() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-requests-verify-false",
			Description: "`requests.get|post|put|delete|...(verify=False)` — disables TLS certificate verification. Any MITM with a self-signed cert intercepts the request.",
			Severity:    "error",
			CWE:         "CWE-295",
			OWASP:       "A02:2021-Cryptographic Failures",
			Tags:        []string{"tls", "no-verify"},
			References:  []string{"bandit:B501"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)
                                arguments: (argument_list (keyword_argument name: (identifier) @kw value: (false)))) @match
                              (#match? @mod "^(requests|httpx)$") (#match? @attr "^(get|post|put|delete|patch|head|options|request|Session)$")
                              (#eq? @kw "verify"))`,
			},
		},
		sastRule{
			Name:        "py-urlopen-file-scheme",
			Description: "`urllib.urlopen(...)` / `urllib.request.urlopen(...)` / `urllib2.urlopen(...)` accepts any URL — `file://` reads local files, `http://attacker/` is SSRF. Validate the scheme allow-list.",
			Severity:    "warning",
			CWE:         "CWE-918",
			OWASP:       "A10:2021-Server-Side Request Forgery (SSRF)",
			Tags:        []string{"ssrf"},
			References:  []string{"bandit:B310"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)) @match
				                 (#match? @mod "^(urllib|urllib2|urllib3|request)$") (#match? @attr "^(urlopen|urlretrieve)$"))
				                ((call function: (attribute object: (attribute attribute: (identifier) @mid) attribute: (identifier) @attr)) @match
				                 (#eq? @mid "request") (#match? @attr "^(urlopen|urlretrieve)$"))`,
			},
		},
		sastRule{
			Name:        "py-requests-host-validation",
			Description: "`requests.get(<dynamic url>, ...)` — flag every request whose URL flows from a non-literal expression. The agent should verify SSRF is impossible at every site (allow-list of hosts, fixed-URL pattern).",
			Severity:    "info",
			CWE:         "CWE-918",
			Tags:        []string{"ssrf", "audit-hook"},
			Pat: map[string]string{
				"python": `((call function: (attribute object: (identifier) @mod attribute: (identifier) @attr)
                                arguments: (argument_list . (call) @url)) @match
                              (#match? @mod "^(requests|httpx)$") (#match? @attr "^(get|post|put|delete|patch|head|options|request)$"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 21. Paramiko / SSH
// ---------------------------------------------------------------------------

func registerPythonParamiko() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-paramiko-exec-command",
			Description: "`paramiko.SSHClient.exec_command(cmd)` — the remote shell parses `cmd`. Any caller-controlled portion of `cmd` is remote command injection.",
			Severity:    "warning",
			CWE:         "CWE-78",
			Tags:        []string{"command-injection", "ssh"},
			Pat: map[string]string{
				"python": `((call function: (attribute attribute: (identifier) @fn)) @match (#eq? @fn "exec_command"))`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// 22. Exception handling — try/except: pass, try/except: continue  (CWE-703)
// ---------------------------------------------------------------------------

func registerPythonExceptionHandling() {
	mustRegisterSAST(
		sastRule{
			Name:        "py-try-except-continue",
			Description: "`except: continue` — silently swallows every exception inside a loop. Production debugging gets nothing; consider logging or narrowing the except.",
			Severity:    "info",
			CWE:         "CWE-703",
			Tags:        []string{"exception", "swallow"},
			References:  []string{"bandit:B112"},
			Pat: map[string]string{
				"python": `((except_clause (block (continue_statement) @body)) @match)`,
			},
		},
	)
}

// ---------------------------------------------------------------------------
// helpers used by PostFilters above — small lower-case wrappers around
// strings.ToLower / strings.Contains so the rule files don't repeat
// the import.
// ---------------------------------------------------------------------------

func trimQuotes(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' || first == '\'' || first == '`') && first == last {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func lower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func containsStr(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	last := len(haystack) - len(needle)
	for i := 0; i <= last; i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
