from setuptools import find_packages, setup
from pathlib import Path

VERSION = '0.4.0'

# 读取 README 作为长描述
this_directory = Path(__file__).parent
long_description = (this_directory / "README.md").read_text(encoding='utf-8')

setup(
    name='gate-ws',
    version=VERSION,
    packages=find_packages(),
    url='https://github.com/gate/gatews',
    install_requires=['websockets>=8.1'],
    license='MIT License',
    author='gateio',
    keywords=["Gate WebSocket V4", "Gate.io", "cryptocurrency", "websocket"],
    author_email='dev@mail.gate.io',
    description='Gate.io WebSocket V4 Python SDK',
    long_description=long_description,
    long_description_content_type='text/markdown',
    python_requires='>=3.6',
    classifiers=[
        'Development Status :: 4 - Beta',
        'Intended Audience :: Developers',
        'License :: OSI Approved :: MIT License',
        'Programming Language :: Python :: 3',
        'Programming Language :: Python :: 3.6',
        'Programming Language :: Python :: 3.7',
        'Programming Language :: Python :: 3.8',
        'Programming Language :: Python :: 3.9',
        'Programming Language :: Python :: 3.10',
        'Programming Language :: Python :: 3.11',
        'Topic :: Software Development :: Libraries :: Python Modules',
    ],
)
