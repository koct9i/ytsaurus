# Generated by devtools/yamaker (pypi).

PY3_LIBRARY()

VERSION(2.2.1)

LICENSE(BSD-3-Clause)

NO_LINT()

NO_CHECK_IMPORTS(
    cloudpickle.cloudpickle_fast
)

PY_SRCS(
    TOP_LEVEL
    cloudpickle/__init__.py
    cloudpickle/cloudpickle.py
    cloudpickle/cloudpickle_fast.py
    cloudpickle/compat.py
)

RESOURCE_FILES(
    PREFIX contrib/python/cloudpickle/py3/
    .dist-info/METADATA
    .dist-info/top_level.txt
)

END()