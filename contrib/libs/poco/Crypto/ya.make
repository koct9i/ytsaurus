# Generated by devtools/yamaker.

LIBRARY()

LICENSE(BSL-1.0)

LICENSE_TEXTS(.yandex_meta/licenses.list.txt)

PEERDIR(
    contrib/libs/openssl
    contrib/libs/poco/Foundation
)

ADDINCL(
    GLOBAL contrib/libs/poco/Crypto/include
    contrib/libs/poco/Crypto/src
    contrib/libs/poco/Foundation/include
)

NO_COMPILER_WARNINGS()

NO_UTIL()

CFLAGS(
    -DPOCO_ENABLE_CPP11
    -DPOCO_ENABLE_CPP14
    -DPOCO_NO_AUTOMATIC_LIBS
    -DPOCO_UNBUNDLED
)

IF (OS_DARWIN)
    CFLAGS(
        -DPOCO_OS_FAMILY_UNIX
        -DPOCO_NO_STAT64
    )
ELSEIF (OS_LINUX)
    CFLAGS(
        -DPOCO_OS_FAMILY_UNIX
        -DPOCO_HAVE_FD_EPOLL
    )
ELSEIF (OS_WINDOWS)
    CFLAGS(
        -DPOCO_OS_FAMILY_WINDOWS
    )
ENDIF()

SRCS(
    src/Cipher.cpp
    src/CipherFactory.cpp
    src/CipherImpl.cpp
    src/CipherKey.cpp
    src/CipherKeyImpl.cpp
    src/CryptoException.cpp
    src/CryptoStream.cpp
    src/CryptoTransform.cpp
    src/DigestEngine.cpp
    src/ECDSADigestEngine.cpp
    src/ECKey.cpp
    src/ECKeyImpl.cpp
    src/EVPPKey.cpp
    src/KeyPair.cpp
    src/KeyPairImpl.cpp
    src/OpenSSLInitializer.cpp
    src/PKCS12Container.cpp
    src/RSACipherImpl.cpp
    src/RSADigestEngine.cpp
    src/RSAKey.cpp
    src/RSAKeyImpl.cpp
    src/X509Certificate.cpp
)

END()