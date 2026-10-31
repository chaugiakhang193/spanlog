import { AsyncLocalStorage } from 'node:async_hooks';
import type {
  AfterQueryEvent,
  BeforeQueryEvent,
  DataSource,
  EntitySubscriberInterface,
} from 'typeorm';
import type { QuerySpan, Spanlog } from './spanlog.js';

/**
 * logQueries adds a subscriber to dataSource that writes a START line before
 * each SQL statement and an end line after it, in the frame that runs the
 * statement. Call it after dataSource.initialize(), which replaces the list
 * of subscribers. Statements run by initialize itself, such as schema
 * synchronization, are therefore not written. It returns a function that
 * removes the subscriber. Each call adds one subscriber, and two calls on one
 * DataSource write every line twice. A statement still running when the
 * subscriber is removed keeps its START line without an end line. When the returned
 * function removes the subscriber, it replaces dataSource.subscribers with a new
 * array. An event already being broadcast finishes iterating its original array, and
 * later broadcasts read the current property. Code that adds a subscriber after that
 * call reads the property again.
 *
 * A statement that another subscriber stops in beforeQuery also keeps its START
 * line without an end line. So does the attempt that CockroachDB fails with error
 * 40001 inside a transaction, because TypeORM 1.1 retries it without an afterQuery.
 *
 * In TypeORM 1.1 the SQL.js and better-sqlite3 drivers reach beforeQuery without
 * an await. A subscriber that issues a statement synchronously from a query event
 * there takes the place of the statement around it, and that statement keeps its
 * START line without an end line. When the two statements have the same text, the
 * result of the outer one can end the inner one.
 *
 * The lines come from the query events of TypeORM. QueryRunner.stream() fires
 * none of them. In TypeORM 1.1 neither does the Aurora Data API driver, and its
 * statements write no lines. spanlog runs on Node.js, which leaves out the drivers
 * for React Native, Expo, Cordova, Capacitor and NativeScript.
 *
 * The spans of createSpanlog never throw. TypeORM rejects the statement when a
 * span of another Spanlog implementation throws from end or fail.
 */
export function logQueries(dataSource: DataSource, log: Spanlog): () => void {
  if (!dataSource.isInitialized) {
    throw new Error(
      'spanlog: logQueries needs an initialized DataSource. Call it after `await dataSource.initialize()`.',
    );
  }
  const subscriber = new QuerySubscriber(log);
  dataSource.subscribers.push(subscriber);
  return () => {
    // TypeORM 1.1 runs forEach over this array during a query event. A splice from
    // a sink or a subscriber there makes the next subscriber miss the event. The
    // array is readonly in the types of DataSource, and initialize replaces it the
    // same way. After such an initialize the array no longer holds this subscriber,
    // and dispose still has to run.
    if (dataSource.subscribers.includes(subscriber)) {
      Object.assign(dataSource, {
        subscribers: dataSource.subscribers.filter(
          (other) => other !== subscriber,
        ),
      });
    }
    subscriber.dispose();
  };
}

interface RunningQuery {
  query: string;
  span: QuerySpan;
}

// TypeORM calls beforeQuery from inside QueryRunner.query, in the async context of
// the code that issued the statement. startQuery therefore sees the frame of the
// caller. Every Node.js driver that fires query events awaits the broadcast of
// beforeQuery before it runs the statement, and that await carries the store of
// enterWith to afterQuery.
class QuerySubscriber implements EntitySubscriberInterface {
  // runningQuery carries a span from beforeQuery to afterQuery of one call of
  // QueryRunner.query. Neither event identifies the call. Two statements can
  // share a text and a parameters array. A driver can map the array to a new one
  // in between. Another subscriber can delay a statement or stop it. No queue
  // order matches every one of these. TypeORM gives two separate events and no
  // callback around the statement, and run() needs that callback. enterWith can
  // leave the span in the code that called query, and only this subscriber reads
  // the store.
  private readonly runningQuery = new AsyncLocalStorage<RunningQuery>();
  private disposed = false;

  constructor(private readonly log: Spanlog) {}

  // A running forEach of TypeORM still reaches this subscriber after a detach from
  // the subscriber before it. A sink can also call detach while startQuery writes
  // the START line. enterWith after a detach would enable the disabled store again.
  beforeQuery(event: BeforeQueryEvent): void {
    if (this.disposed) {
      return;
    }
    const span = this.log.startQuery(event.query);
    if (this.disposed) {
      return;
    }
    this.runningQuery.enterWith({ query: event.query, span });
  }

  // A running forEach of TypeORM also reaches this method after a detach. With
  // AsyncContextFrame, a detach from another async context leaves the store of this
  // context in place. A store that holds another text belongs to a statement issued
  // from a query event of this one. Its span waits for its own afterQuery.
  afterQuery(event: AfterQueryEvent): void {
    if (this.disposed) {
      return;
    }
    const running = this.runningQuery.getStore();
    if (running?.query !== event.query) {
      return;
    }
    if (event.success) {
      running.span.end();
    } else {
      running.span.fail(event.error);
    }
  }

  // Without AsyncContextFrame, the default on Node 22, an AsyncLocalStorage stays
  // reachable until disable() is called.
  dispose(): void {
    if (this.disposed) {
      return;
    }
    this.disposed = true;
    this.runningQuery.disable();
  }
}
