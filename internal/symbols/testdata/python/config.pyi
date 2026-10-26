import sys
from typing import overload

from .base import Base

__all__ = ["Config", "load", "VERSION"]

VERSION: str
LIMIT: int

class Config(Base):
    name: str
    port: int
    @overload
    def get(self, key: str) -> str: ...
    @overload
    def get(self, key: str, default: int) -> int: ...
    if sys.version_info >= (3, 11):
        def reload(self) -> None: ...

@overload
def load(path: str) -> Config: ...
@overload
def load(path: bytes) -> Config: ...

def helper() -> None: ...
