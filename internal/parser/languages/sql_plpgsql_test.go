package languages

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// plpgsqlFunction renders the issue #847 reproduction with body as the
// only statement between BEGIN and END.
func plpgsqlFunction(name, body string) string {
	return fmt.Sprintf(`CREATE OR REPLACE FUNCTION public.%s(p_id uuid)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
  v_rec RECORD;
  v_n int;
BEGIN
  %s
END;
$$;
`, name, body)
}

// A plpgsql CREATE FUNCTION is indexed whatever statement its body
// holds. The grammar cannot read PERFORM, IF NOT FOUND, IF NOT EXISTS
// (…) or IF … IS DISTINCT FROM, and used to turn the whole statement
// into an ERROR node that dropped the function symbol (#847).
func TestSQLExtractor_PlpgsqlBodyNeverDropsFunction(t *testing.T) {
	bodies := map[string]string{
		"return":            `RETURN;`,
		"assign":            `v_n := 1; RETURN;`,
		"select_into":       `SELECT t.a INTO v_rec FROM public.t t WHERE t.id = p_id;`,
		"if_is_null":        `IF p_id IS NULL THEN RETURN; END IF;`,
		"insert":            `INSERT INTO public.t (id) VALUES (p_id);`,
		"upsert":            `INSERT INTO public.t (id, a) VALUES (p_id, 'x') ON CONFLICT ON CONSTRAINT t_pkey DO UPDATE SET a = 'x' WHERE public.t.a IS DISTINCT FROM 'x';`,
		"perform_literal":   `PERFORM 1;`,
		"perform_call":      `PERFORM public.other_fn(p_id);`,
		"if_not_found":      `IF NOT FOUND THEN RETURN; END IF;`,
		"if_not_exists":     `IF NOT EXISTS (SELECT 1 FROM public.t WHERE id = p_id) THEN RETURN; END IF;`,
		"if_distinct_from":  `IF v_rec.a IS DISTINCT FROM 'x' THEN RETURN; END IF;`,
		"dollar_in_comment": `RETURN; -- a stray $$ in a comment`,
	}
	for label, body := range bodies {
		t.Run(label, func(t *testing.T) {
			name := "w7_" + label
			result, err := NewSQLExtractor().Extract("db/migrations/001_fn.sql", []byte(plpgsqlFunction(name, body)))
			require.NoError(t, err)

			fn := nodeNamed(t, result.Nodes, graph.KindFunction, name)
			assert.Equal(t, "db/migrations/001_fn.sql::public."+name, fn.ID)
			assert.Equal(t, "public", fn.Meta["schema"])
			// The node spans the header through the closing `$$;`.
			assert.Equal(t, 1, fn.StartLine)
			assert.Equal(t, 11, fn.EndLine)
		})
	}
}

// A file with several functions keeps every one of them, including when
// the earlier bodies are ones the grammar cannot read, and the line
// ranges of each stay on the original source.
func TestSQLExtractor_PlpgsqlMultipleFunctionsInOneFile(t *testing.T) {
	src := plpgsqlFunction("first_fn", `IF NOT FOUND THEN RETURN; END IF;
  IF v_rec.a IS DISTINCT FROM 'x' THEN RETURN; END IF;
  IF NOT EXISTS (SELECT 1 FROM public.t WHERE id = p_id) THEN RETURN; END IF;`) +
		"\n" + plpgsqlFunction("second_fn", `PERFORM 1;`) +
		"\n" + plpgsqlFunction("third_fn", `PERFORM public.other_fn(p_id);`) +
		"\nCREATE TABLE public.after_fns (id uuid PRIMARY KEY);\n"
	result, err := NewSQLExtractor().Extract("db/migrations/002_fns.sql", []byte(src))
	require.NoError(t, err)

	assert.ElementsMatch(t,
		[]string{"first_fn", "second_fn", "third_fn"},
		sqlNodeNames(result.Nodes, graph.KindFunction))
	assert.Equal(t, []string{"after_fns"}, sqlNodeNames(result.Nodes, graph.KindType))

	first := nodeNamed(t, result.Nodes, graph.KindFunction, "first_fn")
	assert.Equal(t, 1, first.StartLine)
	assert.Equal(t, 13, first.EndLine)
	second := nodeNamed(t, result.Nodes, graph.KindFunction, "second_fn")
	assert.Equal(t, 15, second.StartLine)
	assert.Equal(t, 25, second.EndLine)
	third := nodeNamed(t, result.Nodes, graph.KindFunction, "third_fn")
	assert.Equal(t, 27, third.StartLine)
	assert.Equal(t, 37, third.EndLine)
}

// A tagged dollar quote with trailing header clauses, and a procedure,
// are indexed with the same unreadable body.
func TestSQLExtractor_PlpgsqlTaggedQuoteAndProcedure(t *testing.T) {
	src := []byte(`CREATE OR REPLACE FUNCTION public.touch_row()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path TO 'public', 'pg_temp'
AS $function$
BEGIN
  PERFORM 1;
  IF NOT FOUND THEN RETURN NEW; END IF;
  RETURN NEW;
END;
$function$;

CREATE OR REPLACE PROCEDURE public.archive_rows(p_before timestamptz)
LANGUAGE plpgsql
AS $proc$
BEGIN
  PERFORM public.other_fn(p_before);
  IF NOT FOUND THEN RETURN; END IF;
END;
$proc$;
`)
	result, err := NewSQLExtractor().Extract("db/migrations/003_fns.sql", src)
	require.NoError(t, err)

	fn := nodeNamed(t, result.Nodes, graph.KindFunction, "touch_row")
	assert.Equal(t, 1, fn.StartLine)
	assert.Equal(t, 12, fn.EndLine)
	assert.Nil(t, fn.Meta["sql_type"])

	proc := nodeNamed(t, result.Nodes, graph.KindFunction, "archive_rows")
	assert.Equal(t, "procedure", proc.Meta["sql_type"])
	assert.Equal(t, "public", proc.Meta["schema"])
	assert.Equal(t, 14, proc.StartLine)
	assert.Equal(t, 21, proc.EndLine)
}

func TestMaskSQLDollarQuotedBodies(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"no dollar", "SELECT 1;", "SELECT 1;"},
		{"plain", "AS $$ab\ncd$$;", "AS $$  \n  $$;"},
		{"tagged", "AS $fn$ x $$ y $fn$;", "AS $fn$        $fn$;"},
		{"positional param", "SELECT $1, $2;", "SELECT $1, $2;"},
		{"identifier with dollar", "SELECT a$$b$$;", "SELECT a$$b$$;"},
		{"in single quotes", "SELECT '$$x$$';", "SELECT '$$x$$';"},
		{"escaped quote", "SELECT 'it''s $$', $$z$$;", "SELECT 'it''s $$', $$ $$;"},
		{"in line comment", "-- $$\nAS $$q$$;", "-- $$\nAS $$ $$;"},
		{"in block comment", "/* $$ /* $$ */ */ $$q$$", "/* $$ /* $$ */ */ $$ $$"},
		{"unterminated", "AS $$ oops", "AS $$ oops"},
		{"two bodies", "$a$x$a$ $$y$$", "$a$ $a$ $$ $$"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskSQLDollarQuotedBodies([]byte(tc.src))
			assert.Equal(t, tc.want, string(got))
			assert.Len(t, got, len(tc.src))
		})
	}

	// The input is never modified in place.
	src := []byte("AS $$body$$;")
	orig := bytes.Clone(src)
	_ = maskSQLDollarQuotedBodies(src)
	assert.Equal(t, orig, src)
}
