PY3_PROGRAM(
    yt-map-bench
)

PY_SRCS(
    __init__.py
    __main__.py
    prepare.py
    run.py
)

PEERDIR(
    contrib/python/click
    yt/python/client
    yt/python/yt
    yt/python/yt/wrapper
)

END()
