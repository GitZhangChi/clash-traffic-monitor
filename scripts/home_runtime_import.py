"""Load the hyphenated standalone launcher without executing it."""
import importlib.util
import json
from pathlib import Path


def load_runtime_settings(config_path):
    path = Path(__file__).with_name("home-runtime.py")
    spec = importlib.util.spec_from_file_location("home_runtime", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.router_settings(json.loads(Path(config_path).read_text()))
