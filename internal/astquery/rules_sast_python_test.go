package astquery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPythonRulesCompile drives each registered Python SAST rule
// against a stub source so we catch tree-sitter pattern compile
// errors at unit-test time rather than waiting for an end-to-end
// `analyze sast` call.
func TestPythonRulesCompile(t *testing.T) {
	const stub = "x = 1\n"
	for _, info := range DescribeDetectors() {
		if info.Category != CategorySAST {
			continue
		}
		hasPython := false
		for _, l := range info.Languages {
			if l == "python" {
				hasPython = true
				break
			}
		}
		if !hasPython {
			continue
		}
		t.Run(info.Name, func(t *testing.T) {
			res, err := RunOnSource(context.Background(), Options{Detector: info.Name},
				"sample.py", "python", []byte(stub))
			require.NoError(t, err, "rule %s pattern failed to compile", info.Name)
			_ = res
		})
	}
}

// TestPythonSQLiFStringConstants pins the f-string SQLi PostFilter: an
// interpolation of a PEP 8 constant, or of a loop variable drawn from one, is
// a table/column identifier (which placeholders cannot bind), not injection.
// Any other interpolated expression must still fire.
func TestPythonSQLiFStringConstants(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{"module constant", `COLUMNS = "id, name"
conn.execute(f"SELECT {COLUMNS} FROM t WHERE id = ?", (i,))`, 0},
		{"private constant", `conn.execute(f"SELECT {_COLUMNS} FROM t")`, 0},
		{"attribute constant", `conn.execute(f"SELECT {schema.COLUMNS} FROM t")`, 0},
		{"loop over constant", `for column, sql_type in _ADDED_COLUMNS:
    conn.execute(f"ALTER TABLE t ADD COLUMN {column} {sql_type}")`, 0},
		{"nested loop target", `for i, (column, kind) in PAIRS:
    conn.execute(f"ALTER TABLE t ADD COLUMN {column} {kind}")`, 0},
		{"sqlalchemy text constant", `text(f"SELECT {COLUMNS} FROM t")`, 0},

		{"lower-case name", `cursor.execute(f"SELECT * FROM t WHERE x = {v}")`, 1},
		{"single capital", `cursor.execute(f"SELECT * FROM t WHERE x = {Q}")`, 1},
		{"one constant, one value", `cursor.execute(f"SELECT {COLUMNS} FROM t WHERE x = {v}")`, 1},
		{"call", `cursor.execute(f"SELECT * FROM t WHERE x = {get(v)}")`, 1},
		{"subscript of constant", `cursor.execute(f"SELECT * FROM t WHERE x = {ROWS[i]}")`, 1},
		{"loop over non-constant", `for column in request.args:
    conn.execute(f"ALTER TABLE t ADD COLUMN {column}")`, 1},
		{"loop over constant, other name", `for column in COLUMNS:
    conn.execute(f"SELECT {column} FROM t WHERE x = {v}")`, 1},
		{"inner loop shadows constant loop", `for column in COLUMNS:
    for column in user_columns:
        conn.execute(f"SELECT {column} FROM t")`, 1},
		{"loop outside the function", `for column in COLUMNS:
    def f(column):
        conn.execute(f"SELECT {column} FROM t")`, 1},
		{"sqlalchemy text value", `text(f"SELECT * FROM t WHERE x = {v}")`, 1},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			rule := "py-sqli-execute-fstring"
			if len(c.src) >= 5 && c.src[:5] == "text(" {
				rule = "py-sqlalchemy-text-with-fstring"
			}
			res := runDetector(t, rule, "python", "case.py", c.src)
			require.Equal(t, c.want, res.Total, "%s on:\n%s", rule, c.src)
		})
	}
}

// Per-rule firing tests. Table-driven: each entry pairs the rule
// name with (a) source that MUST trigger the rule and (b) source
// that MUST NOT trigger the rule.
func TestPythonRulesFire(t *testing.T) {
	cases := []struct {
		name string
		bad  string
		good string
	}{
		// --- Injection
		{"py-eval-use",
			`eval("1+1")`,
			`import ast; ast.literal_eval("1+1")`},
		{"py-exec-use",
			`exec("print(1)")`,
			`print(1)`},
		{"py-compile-use",
			`code = compile("print(1)", "<str>", "exec")`,
			`code = 'print(1)'`},

		// --- Deserialisation
		{"py-pickle-load",
			`import pickle
data = pickle.loads(blob)`,
			`import json
data = json.loads(blob)`},
		{"py-marshal-load",
			`import marshal
data = marshal.loads(blob)`,
			`import json
data = json.loads(blob)`},
		{"py-yaml-load-unsafe",
			`import yaml
cfg = yaml.load(text)`,
			`import yaml
cfg = yaml.safe_load(text)`},
		{"py-shelve-open",
			`import shelve
db = shelve.open("path")`,
			`import json
db = json.load(open("path"))`},

		// --- Subprocess
		{"py-subprocess-shell-true",
			`import subprocess
subprocess.run(cmd, shell=True)`,
			`import subprocess
subprocess.run(["ls", "-la"], shell=False)`},
		{"py-os-system",
			`import os
os.system("ls " + d)`,
			`import subprocess
subprocess.run(["ls", d])`},
		{"py-os-popen",
			`import os
os.popen("ls").read()`,
			`import subprocess
subprocess.check_output(["ls"])`},
		{"py-os-spawn",
			`import os
os.spawnl(os.P_WAIT, "/bin/ls", "ls")`,
			`import subprocess
subprocess.run(["ls"])`},

		// --- Crypto
		{"py-hashlib-weak-direct",
			`import hashlib
h = hashlib.md5(b"x")`,
			`import hashlib
h = hashlib.sha256(b"x")`},
		{"py-hashlib-new-weak",
			`import hashlib
h = hashlib.new("md5")`,
			`import hashlib
h = hashlib.new("sha256")`},
		{"py-pycryptodome-weak-hash",
			`from Crypto.Hash import MD5`,
			`from Crypto.Hash import SHA256`},
		{"py-weak-cipher-pycrypto",
			`from Crypto.Cipher import DES`,
			`from Crypto.Cipher import AES`},
		{"py-aes-mode-ecb",
			`from Crypto.Cipher import AES
c = AES.new(key, AES.MODE_ECB)`,
			`from Crypto.Cipher import AES
c = AES.new(key, AES.MODE_GCM, nonce)`},

		// --- Network
		{"py-telnetlib-import",
			`import telnetlib`,
			`import paramiko`},
		{"py-ftplib-import",
			`import ftplib`,
			`from ftplib import FTP_TLS`},
		{"py-imaplib-no-starttls",
			`import imaplib
m = imaplib.IMAP4(host)`,
			`import imaplib
m = imaplib.IMAP4_SSL(host)`},
		{"py-poplib-no-tls",
			`import poplib
p = poplib.POP3(host)`,
			`import poplib
p = poplib.POP3_SSL(host)`},
		{"py-smtplib-no-starttls",
			`import smtplib
s = smtplib.SMTP(host)`,
			`import smtplib
s = smtplib.SMTP_SSL(host)`},

		// --- SSL/TLS
		{"py-ssl-unverified-context",
			`import ssl
ctx = ssl._create_unverified_context()`,
			`import ssl
ctx = ssl.create_default_context()`},
		{"py-ssl-no-verify",
			`import ssl
ctx.verify_mode = ssl.CERT_NONE`,
			`import ssl
ctx.verify_mode = ssl.CERT_REQUIRED`},
		{"py-ssl-old-protocol",
			`import ssl
ctx = ssl.SSLContext(ssl.PROTOCOL_TLSv1)`,
			`import ssl
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)`},
		{"py-paramiko-autoadd-policy",
			`import paramiko
c = paramiko.SSHClient()
c.set_missing_host_key_policy(paramiko.AutoAddPolicy())`,
			`import paramiko
c = paramiko.SSHClient()
c.set_missing_host_key_policy(paramiko.RejectPolicy())`},

		// --- XML
		{"py-xml-etree-no-defusedxml",
			`from xml.etree import ElementTree
ElementTree.parse(path)`,
			`import defusedxml.ElementTree as ET
ET.parse(path)`},
		{"py-xml-minidom-no-defusedxml",
			`from xml.dom import minidom
minidom.parseString(s)`,
			`from defusedxml import minidom
minidom.parseString(s)`},

		// --- Django / Flask / Jinja
		{"py-django-mark-safe",
			`from django.utils.safestring import mark_safe
html = mark_safe(user_input)`,
			`from django.utils.html import escape
html = escape(user_input)`},
		{"py-django-debug-true",
			`DEBUG = True`,
			`DEBUG = False`},
		{"py-django-allowed-hosts-wildcard",
			`ALLOWED_HOSTS = ['*']`,
			`ALLOWED_HOSTS = ['example.com']`},
		{"py-flask-debug-true",
			`app.run(debug=True)`,
			`app.run(debug=False)`},
		{"py-jinja2-autoescape-false",
			`import jinja2
env = jinja2.Environment(autoescape=False)`,
			`import jinja2
env = jinja2.Environment(autoescape=True)`},

		// --- SQLi
		{"py-sqli-execute-format",
			`cursor.execute("SELECT * FROM t WHERE x = {}".format(v))`,
			`cursor.execute("SELECT * FROM t WHERE x = %s", (v,))`},
		{"py-sqli-execute-percent",
			`cursor.execute("SELECT * FROM t WHERE x = %s" % v)`,
			`cursor.execute("SELECT * FROM t WHERE x = %s", (v,))`},
		{"py-sqli-execute-fstring",
			`cursor.execute(f"SELECT * FROM t WHERE x = {v}")`,
			`cursor.execute("SELECT * FROM t WHERE x = %s", (v,))`},

		// --- Hardcoded credentials
		{"py-hardcoded-credential-keyword-arg",
			`connect(host="db", password="actualSecret123")`,
			`connect(host="db", password=os.environ["DBPASS"])`},
		{"py-hardcoded-credential-default-arg",
			`def login(user, password="actualSecret123"): ...`,
			`def login(user, password=None): ...`},
		{"py-django-secret-key-hardcoded",
			`SECRET_KEY = "actualSecret123Xyz"`,
			`SECRET_KEY = os.environ["DJANGO_SECRET_KEY"]`},

		// --- Random
		{"py-random-for-secret",
			`import random
token = random.randint(0, 1<<63)`,
			`import secrets
token = secrets.randbits(64)`},
		{"py-uuid1-predictable",
			`import uuid
sid = uuid.uuid1()`,
			`import uuid
sid = uuid.uuid4()`},

		// --- Filesystem
		{"py-tempfile-mktemp",
			`import tempfile
name = tempfile.mktemp()`,
			`import tempfile
fd, name = tempfile.mkstemp()`},
		{"py-hardcoded-tmp-path",
			`open("/tmp/foo")`,
			`import tempfile
tempfile.mkstemp()`},

		// --- Archives
		{"py-tarfile-extractall",
			`import tarfile
t = tarfile.open(p)
t.extractall(dest)`,
			`import tarfile
t = tarfile.open(p)
t.extractall(dest, filter="data")`},
		{"py-shutil-unpack-archive-user-input",
			`import shutil
shutil.unpack_archive(p, d)`,
			`# nothing`},

		// --- Imports
		{"py-import-pickle",
			`import pickle`,
			`import json`},
		{"py-import-subprocess",
			`import subprocess`,
			`import json`},

		// --- Logging
		{"py-logging-config-listen",
			`import logging.config
t = logging.config.listen()`,
			`import logging.config
logging.config.dictConfig(cfg)`},

		// --- requests
		{"py-requests-verify-false",
			`import requests
r = requests.get(url, verify=False)`,
			`import requests
r = requests.get(url, verify=True)`},
		{"py-urlopen-file-scheme",
			`from urllib import request
request.urlopen(url)`,
			`# nothing`},

		// --- Paramiko
		{"py-paramiko-exec-command",
			`ssh = paramiko.SSHClient()
ssh.connect(host)
stdin, out, err = ssh.exec_command(cmd)`,
			`# nothing`},

		// --- Exception handling
		{"py-try-except-continue",
			`for x in xs:
    try:
        do(x)
    except Exception:
        continue`,
			`for x in xs:
    try:
        do(x)
    except Exception:
        log.error("oops")`},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			bad := runDetector(t, c.name, "python", "case.py", c.bad)
			require.GreaterOrEqual(t, bad.Total, 1, "rule %q should fire on bad fixture; got 0 matches", c.name)

			good := runDetector(t, c.name, "python", "case.py", c.good)
			require.Equal(t, 0, good.Total, "rule %q should be silent on good fixture; got %d matches", c.name, good.Total)
		})
	}
}
