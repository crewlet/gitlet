"""Configuration loading for the sample service."""

from __future__ import annotations

import os
import os.path as osp
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Iterator, Optional, overload

from . import defaults
from .base import Base, Mixin

if TYPE_CHECKING:
    from .registry import Registry

MAX_RETRIES = 3
TIMEOUT: float = 2.5
_CACHE_SIZE = 128
DEFAULT_NAME: str
log = None


@dataclass
class Config(Base, Mixin, metaclass=Meta):
    """A loaded configuration."""

    name: str
    port: int = 8080
    tags: list[str] = field(default_factory=list)
    _secret: str = ""

    def __init__(self, name: str, port: int = 8080) -> None:
        super().__init__()
        self.name = name
        self.port = port

    @classmethod
    def from_env(cls, prefix: str = "APP_") -> Config:
        return cls(os.environ[prefix + "NAME"])

    @staticmethod
    def parse(text: str) -> "Config":
        return Config(text)

    @property
    def address(self) -> str:
        return f"{self.name}:{self.port}"

    @address.setter
    def address(self, value: str) -> None:
        self.name, port = value.split(":")
        self.port = int(port)

    @address.deleter
    def address(self) -> None:
        self.name = ""

    async def refresh(self, *, force: bool = False) -> None:
        await self._reload(force)

    def _reload(self, force):
        pass

    class Options:
        verbose: bool = False

        def describe(self) -> str:
            return "options"


class _Private:
    def visible(self):
        return 1


@overload
def load(path: str) -> Config: ...
@overload
def load(path: str, strict: bool) -> Config: ...
def load(path, strict=False):
    def helper(x):
        return x

    registry: Registry = defaults.registry()
    return helper(Config.parse(osp.join(path, "config")))


def walk(root: str) -> Iterator[str]:
    for name in os.listdir(root):
        yield name


def _private_helper(*args: Any, **kwargs: Any) -> Optional[Config]:
    return None


def __getattr__(name: str) -> Any:
    raise AttributeError(name)
