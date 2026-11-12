// Package spanlog writes readable call-boundary logs. A call writes one line when
// it starts and one line when it ends, with its duration, into the sink of the
// application.
//
// New returns a Logger. Middleware wraps an http.Handler and writes the lines of
// each request. A method writes its own pair with Start and a deferred done. The
// method needs a named error result, and done is deferred directly so that it sees
// a panic. Call wraps one piece of work under a name.
//
//	logger := spanlog.New(spanlog.Options{Service: "shop"})
//	http.ListenAndServe(address, logger.Middleware(mux))
//
//	func (s *OrderService) HandleOrder(ctx context.Context) (err error) {
//		ctx, done := s.log.Start(ctx, "OrderService", "HandleOrder")
//		defer done(&err)
//		return s.inventory.Reserve(ctx)
//	}
//
// Connector wraps the driver.Connector of any database/sql driver. A database
// opened with sql.OpenDB on the result writes a query line pair for each statement,
// with the operation and the table names and without the SQL text.
//
// PrettySink writes the readable lines and is the default, on stdout. JSONSink
// writes one JSON object per line. SlogSink hands the fields and the message to a
// slog logger.
//
// The line format is shared with the Node.js and Python packages and is described
// in https://github.com/chaugiakhang193/spanlog/blob/main/docs/log-format.md. The
// guide for this package is
// https://github.com/chaugiakhang193/spanlog/blob/main/docs/go.md.
package spanlog
