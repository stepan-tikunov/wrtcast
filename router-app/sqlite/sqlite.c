/*
 * sqlite — a minimal SQLite binding for ucode, for wrtcast's state.
 *
 *   import { open, error } from 'sqlite';
 *   let db = open('/tmp/x.db', 5000);  // busy timeout in ms; null on failure
 *   db.exec('CREATE TABLE ...; ...');  // several statements, no parameters
 *   db.query('SELECT a, b FROM t WHERE c = ?', c);  // [{a, b}, ...]
 *   db.run('UPDATE t SET a = ? WHERE c = ?', a, c); // rows changed
 *   db.error();  // message of the last failure
 *
 * Failures return null and leave the message for error(). Parameters:
 * null, booleans (0/1), numbers, strings, and arrays or objects as JSON.
 * Columns: INTEGER as integers, REAL as doubles, TEXT and BLOB as strings.
 * Built against the ucode headers of the router's ucode and its libsqlite3
 * (scripts/build-sqlite.sh).
 */

#include <stdlib.h>
#include <string.h>
#include <sqlite3.h>

#include "ucode/module.h"

struct db {
	sqlite3 *h;
	char *err;
};

static char *open_err;

static void set_err(char **slot, const char *msg)
{
	free(*slot);
	*slot = msg ? strdup(msg) : NULL;
}

static void close_db(void *ud)
{
	struct db *d = ud;

	if (!d)
		return;
	sqlite3_close_v2(d->h);
	free(d->err);
	free(d);
}

static struct db *this_db(uc_vm_t *vm)
{
	struct db **d = uc_fn_this("sqlite.db");

	return d && *d && (*d)->h ? *d : NULL;
}

static uc_value_t *fail_db(struct db *d)
{
	set_err(&d->err, sqlite3_errmsg(d->h));
	return NULL;
}

static uc_value_t *uc_sqlite_open(uc_vm_t *vm, size_t nargs)
{
	uc_value_t *path = uc_fn_arg(0), *timeout = uc_fn_arg(1);
	struct db *d;
	sqlite3 *h = NULL;

	if (ucv_type(path) != UC_STRING) {
		set_err(&open_err, "path must be a string");
		return NULL;
	}
	if (sqlite3_open_v2(ucv_string_get(path), &h,
	                    SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_URI,
	                    NULL) != SQLITE_OK) {
		set_err(&open_err, h ? sqlite3_errmsg(h) : "out of memory");
		sqlite3_close_v2(h);
		return NULL;
	}
	sqlite3_busy_timeout(h, ucv_type(timeout) == UC_INTEGER ?
	                     (int)ucv_int64_get(timeout) : 5000);
	d = calloc(1, sizeof(*d));
	if (!d) {
		sqlite3_close_v2(h);
		set_err(&open_err, "out of memory");
		return NULL;
	}
	d->h = h;
	return ucv_resource_create(vm, "sqlite.db", d);
}

static uc_value_t *uc_sqlite_error(uc_vm_t *vm, size_t nargs)
{
	return open_err ? ucv_string_new(open_err) : NULL;
}

/* Binds the arguments after the SQL to the statement's parameters. */
static int bind_args(uc_vm_t *vm, size_t nargs, sqlite3_stmt *st)
{
	size_t i;
	int rc = SQLITE_OK;

	for (i = 1; i < nargs && rc == SQLITE_OK; i++) {
		uc_value_t *v = uc_fn_arg(i);
		char *s;

		switch (ucv_type(v)) {
		case UC_NULL:
			rc = sqlite3_bind_null(st, i);
			break;
		case UC_BOOLEAN:
			rc = sqlite3_bind_int(st, i, ucv_boolean_get(v));
			break;
		case UC_INTEGER:
			rc = sqlite3_bind_int64(st, i, ucv_int64_get(v));
			break;
		case UC_DOUBLE:
			rc = sqlite3_bind_double(st, i, ucv_double_get(v));
			break;
		case UC_STRING:
			rc = sqlite3_bind_text(st, i, ucv_string_get(v),
			                       ucv_string_length(v), SQLITE_TRANSIENT);
			break;
		case UC_ARRAY:
		case UC_OBJECT:
			s = ucv_to_jsonstring(vm, v);
			rc = s ? sqlite3_bind_text(st, i, s, -1, free) : SQLITE_NOMEM;
			break;
		default:
			rc = SQLITE_MISMATCH;
		}
	}
	return rc;
}

static uc_value_t *column(sqlite3_stmt *st, int i)
{
	switch (sqlite3_column_type(st, i)) {
	case SQLITE_INTEGER:
		return ucv_int64_new(sqlite3_column_int64(st, i));
	case SQLITE_FLOAT:
		return ucv_double_new(sqlite3_column_double(st, i));
	case SQLITE_TEXT:
		return ucv_string_new_length((const char *)sqlite3_column_text(st, i),
		                             sqlite3_column_bytes(st, i));
	case SQLITE_BLOB:
		return ucv_string_new_length(sqlite3_column_blob(st, i),
		                             sqlite3_column_bytes(st, i));
	default:
		return NULL;
	}
}

/* Runs one statement; rows, when given, collects its result rows. */
static int step_all(uc_vm_t *vm, size_t nargs, struct db *d, uc_value_t *rows)
{
	uc_value_t *sql = uc_fn_arg(0);
	sqlite3_stmt *st = NULL;
	int rc, i, n;

	if (ucv_type(sql) != UC_STRING) {
		set_err(&d->err, "SQL must be a string");
		return SQLITE_MISUSE;
	}
	rc = sqlite3_prepare_v2(d->h, ucv_string_get(sql), ucv_string_length(sql), &st, NULL);
	if (rc == SQLITE_OK && !st)
		return SQLITE_OK; /* only a comment or whitespace */
	if (rc == SQLITE_OK)
		rc = bind_args(vm, nargs, st);
	if (rc != SQLITE_OK) {
		set_err(&d->err, rc == SQLITE_MISMATCH ? "unsupported parameter type" :
		                 sqlite3_errmsg(d->h));
		sqlite3_finalize(st);
		return rc;
	}
	n = sqlite3_column_count(st);
	while ((rc = sqlite3_step(st)) == SQLITE_ROW) {
		uc_value_t *row;

		if (!rows)
			continue;
		row = ucv_object_new(vm);
		for (i = 0; i < n; i++)
			ucv_object_add(row, sqlite3_column_name(st, i), column(st, i));
		ucv_array_push(rows, row);
	}
	if (rc != SQLITE_DONE)
		set_err(&d->err, sqlite3_errmsg(d->h));
	sqlite3_finalize(st);
	return rc == SQLITE_DONE ? SQLITE_OK : rc;
}

static uc_value_t *uc_sqlite_query(uc_vm_t *vm, size_t nargs)
{
	struct db *d = this_db(vm);
	uc_value_t *rows;

	if (!d)
		return NULL;
	rows = ucv_array_new(vm);
	if (step_all(vm, nargs, d, rows) != SQLITE_OK) {
		ucv_put(rows);
		return NULL;
	}
	return rows;
}

static uc_value_t *uc_sqlite_run(uc_vm_t *vm, size_t nargs)
{
	struct db *d = this_db(vm);

	if (!d || step_all(vm, nargs, d, NULL) != SQLITE_OK)
		return NULL;
	return ucv_int64_new(sqlite3_changes(d->h));
}

static uc_value_t *uc_sqlite_exec(uc_vm_t *vm, size_t nargs)
{
	struct db *d = this_db(vm);
	uc_value_t *sql = uc_fn_arg(0);

	if (!d)
		return NULL;
	if (ucv_type(sql) != UC_STRING) {
		set_err(&d->err, "SQL must be a string");
		return NULL;
	}
	if (sqlite3_exec(d->h, ucv_string_get(sql), NULL, NULL, NULL) != SQLITE_OK)
		return fail_db(d);
	return ucv_boolean_new(true);
}

static uc_value_t *uc_sqlite_db_error(uc_vm_t *vm, size_t nargs)
{
	struct db *d = this_db(vm);

	return d && d->err ? ucv_string_new(d->err) : NULL;
}

static uc_value_t *uc_sqlite_close(uc_vm_t *vm, size_t nargs)
{
	struct db **d = uc_fn_this("sqlite.db");

	if (!d || !*d)
		return NULL;
	close_db(*d);
	*d = NULL;
	return ucv_boolean_new(true);
}

static const uc_function_list_t db_fns[] = {
	{ "query", uc_sqlite_query },
	{ "run",   uc_sqlite_run },
	{ "exec",  uc_sqlite_exec },
	{ "error", uc_sqlite_db_error },
	{ "close", uc_sqlite_close },
};

static const uc_function_list_t global_fns[] = {
	{ "open",  uc_sqlite_open },
	{ "error", uc_sqlite_error },
};

void uc_module_init(uc_vm_t *vm, uc_value_t *scope)
{
	uc_function_list_register(scope, global_fns);
	uc_type_declare(vm, "sqlite.db", db_fns, close_db);
}
