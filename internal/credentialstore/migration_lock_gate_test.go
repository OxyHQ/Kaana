package credentialstore

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The migration lock gate: the rules of oxy's `oxy-db-check-migration-locks`
// (oxy MIGRATION-CONTRACT.md, "Every migration keeps its locks short"),
// applied to Kaana's own migrations, which are plain SQL files rather than a
// drizzle journal. The runner bounds a lock to ONE migration; this keeps that
// one migration short.
//
//  1. A FOREIGN KEY added to a table that already exists is NOT VALID, and is
//     validated by ALTER TABLE ... VALIDATE CONSTRAINT in a later migration.
//  2. A migration that takes an ACCESS EXCLUSIVE lock on an existing table
//     (any ALTER TABLE other than a NOT VALID foreign key or a VALIDATE) does
//     nothing slow in the same file: no UPDATE/DELETE/INSERT...SELECT, and no
//     index build on a table the file did not create.
//  3. An index on an existing table is CREATE INDEX IF NOT EXISTS, so it can be
//     built CONCURRENTLY out of band first.
//  4. Every `-- oxy:statement-timeout=` marker is legible.
//
// Every Kaana table sits on the request path, so every existing table is hot.
// A migration reviewed against these rules that must break one says why in its
// own file: `-- oxy:lock-review=<reason>`.

// lockGateBaseline is the last migration applied before the gate existed.
const lockGateBaseline = 22

var (
	lockReviewWaiver = regexp.MustCompile(`(?m)^--\s*oxy:lock-review=\S`)
	gateIdent        = `(?:"?public"?\.)?"?([a-z0-9_]+)"?`
	gateCreateTable  = regexp.MustCompile(`^create (?:unlogged )?table (?:if not exists )?` + gateIdent)
	gateAlterTable   = regexp.MustCompile(`^alter table (?:if exists )?(?:only )?` + gateIdent)
	gateCreateIndex  = regexp.MustCompile(`^create (?:unique )?index (concurrently )?(if not exists )?(?:"?[a-z0-9_]+"? )?on (?:only )?` + gateIdent)
	gateDML          = regexp.MustCompile(`^(update|delete from|insert into)\b`)
	gateForeignKey   = regexp.MustCompile(`\bforeign key\b`)
	gateWhitespace   = regexp.MustCompile(`\s+`)
	gateDollarTag    = regexp.MustCompile(`^\$[A-Za-z_0-9]*\$`)
)

// sqlStatements splits a migration into statements on top-level semicolons,
// skipping comments, quoted strings and dollar-quoted bodies, and returns each
// lower-cased with its whitespace collapsed.
func sqlStatements(body string) []string {
	var statements []string
	var current strings.Builder
	flush := func() {
		statement := strings.TrimSpace(gateWhitespace.ReplaceAllString(current.String(), " "))
		if statement != "" {
			statements = append(statements, strings.ToLower(statement))
		}
		current.Reset()
	}
	for i := 0; i < len(body); {
		switch {
		case strings.HasPrefix(body[i:], "--"):
			end := strings.IndexByte(body[i:], '\n')
			if end < 0 {
				i = len(body)
			} else {
				i += end
			}
		case strings.HasPrefix(body[i:], "/*"):
			end := strings.Index(body[i+2:], "*/")
			if end < 0 {
				i = len(body)
			} else {
				i += end + 4
			}
			current.WriteByte(' ')
		case body[i] == '\'':
			end := i + 1
			for end < len(body) {
				if body[end] == '\'' {
					if end+1 < len(body) && body[end+1] == '\'' {
						end += 2
						continue
					}
					break
				}
				end++
			}
			current.WriteString(body[i:min(end+1, len(body))])
			i = end + 1
		case body[i] == '$' && gateDollarTag.MatchString(body[i:]):
			tag := gateDollarTag.FindString(body[i:])
			end := strings.Index(body[i+len(tag):], tag)
			if end < 0 {
				current.WriteString(body[i:])
				i = len(body)
			} else {
				current.WriteString(body[i : i+len(tag)+end+len(tag)])
				i += len(tag) + end + len(tag)
			}
		case body[i] == ';':
			flush()
			i++
		default:
			current.WriteByte(body[i])
			i++
		}
	}
	flush()
	return statements
}

func endsNotValid(statement string) bool {
	return strings.HasSuffix(strings.TrimSpace(strings.TrimSuffix(statement, ";")), " not valid")
}

// migrationLockProblems returns the gate's findings for one migration.
func migrationLockProblems(version, body string) []string {
	var problems []string
	if _, err := migrationStatementTimeoutFor(version, body); err != nil {
		problems = append(problems, err.Error())
	}
	if lockReviewWaiver.MatchString(body) {
		return problems
	}
	statements := sqlStatements(body)
	created := map[string]bool{}
	for _, statement := range statements {
		if match := gateCreateTable.FindStringSubmatch(statement); match != nil {
			created[match[1]] = true
		}
	}
	var exclusive, slow []string
	for _, statement := range statements {
		short := statement
		if len(short) > 140 {
			short = short[:140] + "..."
		}
		if match := gateAlterTable.FindStringSubmatch(statement); match != nil {
			table := match[1]
			if created[table] {
				continue
			}
			isForeignKey := gateForeignKey.MatchString(statement) && strings.Contains(statement, "add constraint")
			if isForeignKey && !endsNotValid(statement) {
				problems = append(problems, version+": foreign key on existing table "+table+
					" is validated in place; add it NOT VALID and VALIDATE CONSTRAINT in a later migration: "+short)
			}
			notValidForeignKey := isForeignKey && endsNotValid(statement)
			if !notValidForeignKey && !strings.Contains(statement, "validate constraint") && !slices.Contains(exclusive, table) {
				exclusive = append(exclusive, table)
			}
			continue
		}
		if match := gateCreateIndex.FindStringSubmatch(statement); match != nil {
			concurrently, ifNotExists, table := match[1], match[2], match[3]
			if created[table] {
				continue
			}
			if concurrently == "" {
				slow = append(slow, "index build on existing table "+table)
			}
			if ifNotExists == "" {
				problems = append(problems, version+": index on existing table "+table+
					" without IF NOT EXISTS; build it CONCURRENTLY out of band first: "+short)
			}
			continue
		}
		if match := gateDML.FindStringSubmatch(statement); match != nil {
			if match[1] != "insert into" || strings.Contains(statement, "select") {
				slow = append(slow, short)
			}
		}
	}
	if len(exclusive) > 0 && len(slow) > 0 {
		problems = append(problems, version+": takes an ACCESS EXCLUSIVE lock on "+strings.Join(exclusive, ", ")+
			" and holds it while it runs "+strings.Join(slow, "; ")+". Put the DDL in its own migration.")
	}
	return problems
}

func TestEveryNewMigrationKeepsItsLocksShort(t *testing.T) {
	checked := 0
	for _, migration := range schemaMigrations() {
		number, err := strconv.Atoi(migration.version)
		if err != nil {
			t.Fatalf("migration version %q is not a number", migration.version)
		}
		if number <= lockGateBaseline {
			continue
		}
		checked++
		for _, problem := range migrationLockProblems(migration.version, migration.body) {
			t.Error(problem)
		}
	}
	t.Logf("%d migration(s) after %04d checked", checked, lockGateBaseline)
}

func TestMigrationLockGateCatchesEachRule(t *testing.T) {
	for name, testCase := range map[string]struct {
		body  string
		fails bool
	}{
		"foreign key validated in place": {
			body:  `ALTER TABLE provider_credentials ADD CONSTRAINT fk FOREIGN KEY (k) REFERENCES other (k);`,
			fails: true,
		},
		"foreign key added NOT VALID": {
			body: `ALTER TABLE provider_credentials ADD CONSTRAINT fk FOREIGN KEY (k) REFERENCES other (k) NOT VALID;`,
		},
		"backfill beside an ACCESS EXCLUSIVE lock": {
			body:  "ALTER TABLE provider_credentials ADD COLUMN note TEXT;\nUPDATE provider_credentials SET note = 'x';",
			fails: true,
		},
		"index on an existing table without IF NOT EXISTS": {
			body:  `CREATE INDEX provider_credentials_note_idx ON provider_credentials (note);`,
			fails: true,
		},
		"index and backfill on a table the file creates": {
			body: "CREATE TABLE fresh (id TEXT);\nCREATE INDEX fresh_idx ON fresh (id);\nINSERT INTO fresh SELECT key_id FROM provider_credentials;",
		},
		"a semicolon inside a function body is not a statement end": {
			body: "CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $f$ BEGIN UPDATE provider_credentials SET x = 1; END $f$;\nALTER TABLE provider_credentials ADD COLUMN y TEXT;",
		},
		"an illegible statement-timeout marker": {
			body:  "-- oxy:statement-timeout=10m\nSELECT 1;",
			fails: true,
		},
		"a reviewed exception": {
			body: "-- oxy:lock-review=tiny table, measured\nALTER TABLE provider_credentials ADD COLUMN note TEXT;\nUPDATE provider_credentials SET note = 'x';",
		},
	} {
		t.Run(name, func(t *testing.T) {
			problems := migrationLockProblems("9999", testCase.body)
			if (len(problems) > 0) != testCase.fails {
				t.Fatalf("problems = %q, want failure %v", problems, testCase.fails)
			}
		})
	}
}
