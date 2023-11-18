PY3TEST()

TEST_SRCS(
    conftest.py
    test_simple.py
)

INCLUDE(${ARCADIA_ROOT}/yt/yt/tests/integration/YaMakeBoilerplateForTests.txt)

DEPENDS(
    yt/yt/packages/tests_package
    yt/yql/agent/bin
)

# In open source these artifacts must be taken from YDB repo.
IF (NOT OPENSOURCE)
    DEPENDS(
        yt/yql/plugin/dynamic
        contrib/ydb/library/yql/tools/mrjob
        contrib/ydb/library/yql/udfs/common/re2
    )
ENDIF()

PEERDIR(
    yt/yt/tests/conftest_lib
)

END()