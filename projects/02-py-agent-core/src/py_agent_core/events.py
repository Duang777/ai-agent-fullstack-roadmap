from typing import Annotated, Literal

from pydantic import BaseModel, Field, TypeAdapter, ValidationError


class TextEvent(BaseModel):
    type: Literal["text"]
    delta: str


class ToolCallEvent(BaseModel):
    type: Literal["tool_call"]
    id: str
    name: str
    args: str


class Usage(BaseModel):
    input: int
    output: int


class DoneEvent(BaseModel):
    type: Literal["done"]
    usage: Usage


class ErrorEvent(BaseModel):
    type: Literal["error"]
    message: str


StreamEvent = Annotated[
    TextEvent | ToolCallEvent | DoneEvent | ErrorEvent,
    Field(discriminator="type"),
]

_adapter = TypeAdapter(StreamEvent)


def parse_event(data: str) -> tuple[StreamEvent | None, str | None]:
    """返回 (event, None) 或 (None, 错误文本)，永远不抛异常。"""
    try:
        return _adapter.validate_json(data), None
    except ValidationError as e:
        return None, str(e)
