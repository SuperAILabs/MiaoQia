"""Load callables selected by the trusted acquisition launcher."""
from importlib import import_module
import os


def operator_callable(variable: str):
    module, name = os.environ[variable].split(":")
    callback = getattr(import_module(module), name)
    if not callable(callback):
        raise ValueError("trusted callable required")
    return callback
