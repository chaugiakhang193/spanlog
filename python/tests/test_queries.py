from support import make


def test_start_query_names_the_tables_given_to_a_rollback():
    log, sink = make()
    log.start_query("ROLLBACK", rolled_back_tables=("orders", "warehouse.inventory")).end()
    assert sink.msgs == [
        "query ROLLBACK orders, warehouse.inventory | START",
        "query ROLLBACK orders, warehouse.inventory | DONE",
    ]
    assert [rendered.level for rendered in sink.rendered] == ["debug", "warn"]
    assert sink.rendered[1].fields["tables"] == ["orders", "warehouse.inventory"]


def test_start_query_keeps_the_tables_of_other_statements():
    log, sink = make()
    log.start_query("COMMIT", rolled_back_tables=["orders"]).end()
    assert sink.msgs == ["query COMMIT | START", "query COMMIT | DONE"]
