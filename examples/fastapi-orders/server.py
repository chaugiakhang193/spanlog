import asyncio
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse
from pydantic import BaseModel
from spanlog import Spanlog, get_request_id
from spanlog.asgi import LogRequests
from spanlog.sqlalchemy import log_queries
from sqlalchemy import insert, update
from sqlalchemy.exc import SQLAlchemyError
from sqlalchemy.ext.asyncio import AsyncEngine, create_async_engine
from sqlalchemy.orm import DeclarativeBase, Mapped, mapped_column

HOST = "127.0.0.1"
PORT = 3022
PAYMENT_DELAY_SECONDS = 0.6


class Base(DeclarativeBase):
    pass


class Inventory(Base):
    __tablename__ = "inventory"
    sku: Mapped[str] = mapped_column(primary_key=True)
    stock: Mapped[int]


class Order(Base):
    __tablename__ = "orders"
    id: Mapped[int] = mapped_column(primary_key=True)
    sku: Mapped[str]
    quantity: Mapped[int]


class InventoryService:
    def __init__(self, engine: AsyncEngine) -> None:
        self._engine = engine

    async def reserve(self, sku: str, quantity: int) -> None:
        statement = (
            update(Inventory)
            .where(Inventory.sku == sku, Inventory.stock >= quantity)
            .values(stock=Inventory.stock - quantity)
        )
        async with self._engine.begin() as conn:
            result = await conn.execute(statement)
        if result.rowcount == 0:
            raise ValueError("stock not available")


class PaymentService:
    async def charge(self) -> None:
        await asyncio.sleep(PAYMENT_DELAY_SECONDS)


class OrderService:
    def __init__(
        self, engine: AsyncEngine, inventory: InventoryService, payment: PaymentService
    ) -> None:
        self._engine = engine
        self._inventory = inventory
        self._payment = payment

    async def handle_order(self, sku: str, quantity: int) -> dict[str, int]:
        await self.validate(quantity)
        await self._inventory.reserve(sku, quantity)
        await self._payment.charge()
        statement = insert(Order).values(sku=sku, quantity=quantity).returning(Order.id)
        async with self._engine.begin() as conn:
            result = await conn.execute(statement)
        return {"id": result.scalar_one()}

    async def validate(self, quantity: int) -> None:
        if quantity <= 0:
            raise ValueError("quantity must be a positive integer")


log = Spanlog(service="shop", level="debug")

# drop_all drops the inventory and orders tables on every start. The URL is fixed to the
# local database of compose.yaml for that reason.
engine = create_async_engine("postgresql+asyncpg://spanlog:spanlog@127.0.0.1:5438/spanlog")

order_service = log.traced(
    OrderService(
        engine,
        log.traced(InventoryService(engine), "InventoryService"),
        log.traced(PaymentService(), "PaymentService"),
    ),
    "OrderService",
)


@asynccontextmanager
async def lifespan(_app: FastAPI) -> AsyncIterator[None]:
    async with engine.begin() as conn:
        await conn.run_sync(Base.metadata.drop_all)
        await conn.run_sync(Base.metadata.create_all)
        await conn.execute(insert(Inventory).values(sku="book", stock=1))
    log_queries(engine, log)
    yield
    await engine.dispose()


app = FastAPI(lifespan=lifespan)
app.add_middleware(LogRequests, log=log)


class OrderRequest(BaseModel):
    sku: str
    quantity: int


@app.post("/orders", status_code=201)
async def create_order(body: OrderRequest) -> dict[str, int]:
    return await order_service.handle_order(body.sku, body.quantity)


# spanlog has already reported the error. This handler only answers the client. An
# exception that reaches uvicorn is logged with its full text, and the text of a
# SQLAlchemy error holds the SQL and the parameters.
@app.exception_handler(ValueError)
@app.exception_handler(SQLAlchemyError)
async def order_failed(_request: Request, _error: Exception) -> JSONResponse:
    return JSONResponse({"error": "order failed", "requestId": get_request_id()}, status_code=500)


if __name__ == "__main__":
    uvicorn.run(app, host=HOST, port=PORT, access_log=False)
