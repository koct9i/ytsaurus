import yt.logger as yt_logger
import yt.wrapper.default_config as default_config
import yt.wrapper.http_helpers as http
from yt.wrapper.errors import YtConfigError
import yt.wrapper as yt

import pytest

from typing import get_type_hints
from unittest import mock
import os
import subprocess
import tempfile

from yt.testlib import authors


@authors("denvr")
def test_config_types():
    def _check_keys(type_object, config_object):
        type_hints = get_type_hints(type_object)
        for param_name, param_value in config_object.items():
            assert param_name in type_hints, (
                "New config parameter should be described in default_config.DefaultConfigType"
            )
            if isinstance(param_value, dict) and param_value:
                _check_keys(type_hints[param_name], param_value)

    _check_keys(yt.default_config.DefaultConfigType, yt.default_config.default_config)


@authors("denvr")   # author: marydrobotun@gmail.com
def test_log_once():
    with mock.patch.object(yt_logger.LOGGER, "log") as logger_mock, \
            mock.patch.object(yt_logger, "MAX_BUFF_LEN", 5), \
            mock.patch.object(yt_logger, "BUFF_CLEANING_LEN", 2):
        yt_logger.log_once(30, "test1")
        yt_logger.log_once(30, "test1")
        yt_logger.log_once(30, "test2")
        yt_logger.log_once(30, "test3")
        yt_logger.log_once(30, "test4")
        assert len(yt_logger.LOG_ONCE_BUFF) == 4
        yt_logger.log_once(30, "test5")
        assert logger_mock.call_count == 5
        assert len(yt_logger.LOG_ONCE_BUFF) == 3, "clean"
        assert logger_mock.call_count == 5, "hit after clean"
        yt_logger.log_once(30, "test5")
        assert logger_mock.call_count == 5, "hit after clean"
        yt_logger.log_once(30, "test1")
        assert logger_mock.call_count == 6, "miss after clean"


@authors("koct9i")
def test_token_command():
    client = yt.YtClient(config={
        "token_command": ["token-helper", "get"],
        "token_command_timeout": 3210,
    })

    with mock.patch.object(http.subprocess, "run") as run_mock:
        run_mock.return_value = subprocess.CompletedProcess(
            ["token-helper", "get"],
            0,
            stdout="command-token\n",
            stderr="",
        )

        assert http.get_token(client=client) == "command-token"
        assert http.get_token(client=client) == "command-token"

    assert run_mock.call_count == 1
    assert run_mock.call_args.args[0] == ["token-helper", "get"]
    assert run_mock.call_args.kwargs["timeout"] == pytest.approx(3.21)
    assert run_mock.call_args.kwargs["stdin"] is subprocess.DEVNULL

    with mock.patch.object(http.subprocess, "run") as run_mock:
        run_mock.return_value = subprocess.CompletedProcess(
            ["token-helper", "get"],
            0,
            stdout="command-token",
            stderr="",
        )
        assert http.get_token(client=yt.YtClient(config={"token_command": ["token-helper", "get"]})) == "command-token"


@authors("koct9i")
def test_token_command_precedence():
    with tempfile.NamedTemporaryFile(mode="w", delete=False) as token_file:
        token_file.write("file-token")

    try:
        client = yt.YtClient(config={
            "token_command": "token-helper get --cluster hahn",
            "token_path": token_file.name,
        })

        with mock.patch.object(http.subprocess, "run") as run_mock:
            run_mock.return_value = subprocess.CompletedProcess(
                ["token-helper", "get", "--cluster", "hahn"],
                0,
                stdout="command-token\n",
                stderr="",
            )
            assert http.get_token(client=client) == "command-token"

        assert run_mock.call_args.args[0] == ["token-helper", "get", "--cluster", "hahn"]

        config_token_client = yt.YtClient(config={
            "token": "config-token",
            "token_command": ["token-helper", "get"],
            "token_path": token_file.name,
        })

        with mock.patch.object(http.subprocess, "run") as run_mock:
            assert http.get_token(client=config_token_client) == "config-token"
            run_mock.assert_not_called()
    finally:
        os.unlink(token_file.name)


@authors("koct9i")
def test_token_command_errors():
    client = yt.YtClient(config={"token_command": ["token-helper", "get"]})

    with mock.patch.object(
        http.subprocess,
        "run",
        side_effect=subprocess.TimeoutExpired(["token-helper"], 10),
    ):
        with pytest.raises(yt.YtTokenError, match="timed out"):
            http.get_token(client=client)

    with mock.patch.object(http.subprocess, "run") as run_mock:
        run_mock.return_value = subprocess.CompletedProcess(
            ["token-helper", "get"],
            1,
            stdout="",
            stderr="boom",
        )
        with pytest.raises(yt.YtTokenError, match="status 1"):
            http.get_token(client=yt.YtClient(config={"token_command": ["token-helper", "get"]}))

    with mock.patch.object(http.subprocess, "run") as run_mock:
        run_mock.return_value = subprocess.CompletedProcess(
            ["token-helper", "get"],
            0,
            stdout="\n",
            stderr="",
        )
        with pytest.raises(yt.YtTokenError, match="empty stdout"):
            http.get_token(client=yt.YtClient(config={"token_command": ["token-helper", "get"]}))

    with mock.patch.object(http.subprocess, "run") as run_mock:
        run_mock.return_value = subprocess.CompletedProcess(
            ["token-helper", "get"],
            0,
            stdout="token\nextra\n",
            stderr="",
        )
        with pytest.raises(yt.YtTokenError, match="single line"):
            http.get_token(client=yt.YtClient(config={"token_command": ["token-helper", "get"]}))


@authors("koct9i")
def test_token_command_does_not_fallback_to_file():
    with tempfile.NamedTemporaryFile(mode="w", delete=False) as token_file:
        token_file.write("file-token")

    try:
        client = yt.YtClient(config={
            "token_command": ["token-helper", "get"],
            "token_path": token_file.name,
        })

        with mock.patch.object(http.subprocess, "run") as run_mock:
            run_mock.return_value = subprocess.CompletedProcess(
                ["token-helper", "get"],
                1,
                stdout="",
                stderr="boom",
            )
            with pytest.raises(yt.YtTokenError, match="status 1"):
                http.get_token(client=client)
    finally:
        os.unlink(token_file.name)


@authors("koct9i")
def test_token_command_auth_class_conflict():
    client = yt.YtClient(config={
        "token_command": ["token-helper", "get"],
        "auth_class": {
            "module_name": "yt.wrapper.testlib.helpers",
            "class_name": "CustomAuthTest",
        },
    })

    with pytest.raises(YtConfigError, match="Only one of `auth_class` and `token_command`"):
        http.get_token(client=client)


@authors("koct9i")
def test_token_command_env_override():
    with mock.patch.dict(os.environ, {"YT_TOKEN_COMMAND": "token-helper get"}, clear=False):
        config = default_config.get_config_from_env()
        assert config["token_command"] == "token-helper get"
