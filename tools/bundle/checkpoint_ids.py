"""Convert research checkpoint task IDs to the product's P1/P2/P3 IDs.

Only task inventories and anchored tensor-name segments change. Tensor objects and their order stay intact.
The B initialization digest remains the original fingerprint, including the source tensor names.
"""

import copy
import json
import re
from collections import OrderedDict
from typing import Any

TASK_IDS = {"E1": "P1", "E2": "P2", "E5": "P3"}
LEGACY_TASKS = ("E1", "E2", "E5", "TF", "DC", "RP")
PRODUCT_TASKS = ("P1", "P2", "P3", "TF", "DC", "RP")
CONVERSION = "stave-task-ids-e-to-p-1"
_FORMATS = {"stave-flat-1", *(f"stave-structural-{n}" for n in (2, 3, 4, 6))}
_TASK_SEGMENT = re.compile(r"^(roles|heads|cand)\.([^.]+)(?=\.|$)")


def product_task(task: str) -> str:
    """Map one exact research task ID, leaving other identifiers unchanged."""
    return TASK_IDS.get(task, task)


def state_key(name: str) -> str:
    """Map the task segment of roles, heads and cand names, including module metadata."""
    return _TASK_SEGMENT.sub(lambda m: f"{m[1]}.{product_task(m[2])}", name, count=1)


def convert_tasks(tasks: Any) -> list[str]:
    """Convert a nonempty canonical legacy or product inventory; partial inventories are supported."""
    if (
        not isinstance(tasks, (list, tuple))
        or not tasks
        or not all(isinstance(t, str) for t in tasks)
    ):
        raise ValueError("tasks must be a nonempty task inventory")
    legacy = any(t in TASK_IDS for t in tasks)
    if legacy and any(t in TASK_IDS.values() for t in tasks):
        raise ValueError("mixed legacy and product task inventory")
    order = LEGACY_TASKS if legacy else PRODUCT_TASKS
    if list(tasks) != [t for t in order if t in tasks]:
        raise ValueError(f"tasks are not a canonical task inventory: {tasks!r}")
    return [product_task(t) for t in tasks]


def _task_map(value: Any, tasks: list[str], field: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != set(tasks):
        raise ValueError(f"{field} keys do not match the task inventory")
    return {product_task(t): v for t, v in value.items()}


def _check_name(name: Any, tasks: list[str]) -> None:
    if not isinstance(name, str):
        raise TypeError("tensor names must be strings")
    match = _TASK_SEGMENT.match(name)
    if match is not None and match[2] not in tasks:
        raise ValueError(f"tensor name {name!r} does not match the task inventory")


def convert_config(config: dict[str, Any]) -> dict[str, Any]:
    """Copy a saved configuration, converting only its known task-bearing fields."""
    if not isinstance(config, dict) or config.get("format") not in _FORMATS:
        raise ValueError("unsupported checkpoint configuration format")
    converted = convert_tasks(config.get("tasks"))
    tasks: list[str] = config["tasks"]
    out = copy.deepcopy(config)
    out["tasks"] = converted
    if config["format"] == "stave-flat-1":
        out["slots"] = _task_map(out.get("slots"), tasks, "slots")
        init = out.get("init")
        if not isinstance(init, dict) or init.get("tasks") != tasks:
            raise ValueError("init.tasks does not match the task inventory")
        if not isinstance(init.get("sha256"), str):
            raise ValueError(
                "init.sha256 must retain the source initialization fingerprint"
            )
        init["tasks"] = converted.copy()
    else:
        out["roles"] = _task_map(out.get("roles"), tasks, "roles")
        names = out.get("init_order")
        if not isinstance(names, list):
            raise ValueError("init_order must be a list of tensor names")
        for name in names:
            _check_name(name, tasks)
        out["init_order"] = [state_key(name) for name in names]
    return out


def convert_checkpoint(saved: dict[str, Any]) -> dict[str, Any]:
    """Convert a loaded checkpoint without copying or modifying any tensor."""
    import torch

    if not isinstance(saved, dict) or saved.get("format") not in _FORMATS:
        raise ValueError("unsupported checkpoint format")
    try:
        config = json.loads(saved["config_json"])
    except (KeyError, TypeError, json.JSONDecodeError) as exc:
        raise ValueError("checkpoint config_json is not valid JSON") from exc
    converted = convert_config(config)
    if config["format"] != saved["format"]:
        raise ValueError("checkpoint and configuration formats differ")
    source = saved.get("state")
    if not isinstance(source, dict) or not source:
        raise ValueError("checkpoint state must be a nonempty tensor mapping")
    state: Any = OrderedDict()
    for name, tensor in source.items():
        _check_name(name, config["tasks"])
        if not isinstance(tensor, torch.Tensor):
            raise TypeError(f"state value {name!r} is not a tensor")
        state[state_key(name)] = tensor
    source_metadata = getattr(source, "_metadata", None)
    if source_metadata is not None:
        metadata = OrderedDict()
        for name, value in source_metadata.items():
            _check_name(name, config["tasks"])
            metadata[state_key(name)] = copy.deepcopy(value)
        state._metadata = metadata
    return {
        **saved,
        "config_json": json.dumps(converted, sort_keys=True),
        "state": state,
    }
