#!/bin/bash

# Gate-WS Python SDK 发布脚本

set -e  # 遇到错误立即退出

echo "========================================="
echo "Gate-WS Python SDK 发布工具"
echo "========================================="

# 检查是否在正确的目录
if [ ! -f "setup.py" ]; then
    echo "错误: 请在 python 目录下运行此脚本"
    exit 1
fi

# 获取版本号
VERSION=$(grep "VERSION = " setup.py | cut -d "'" -f 2)
echo "当前版本: $VERSION"

# 询问发布到哪里
echo ""
echo "请选择发布目标:"
echo "1) TestPyPI (测试环境，推荐)"
echo "2) PyPI (正式环境)"
echo "3) 两者都发布"
read -p "请输入选项 [1-3]: " choice

# 清理旧文件
echo ""
echo "[1/4] 清理旧的构建文件..."
rm -rf build/ dist/ *.egg-info

# 构建
echo ""
echo "[2/4] 构建发布包..."
python setup.py sdist bdist_wheel

# 检查
echo ""
echo "[3/4] 检查包..."
twine check dist/*

# 上传
echo ""
echo "[4/4] 上传到 PyPI..."

case $choice in
    1)
        echo "上传到 TestPyPI..."
        twine upload --repository testpypi dist/*
        echo ""
        echo "✅ 发布成功!"
        echo "测试安装: pip install --index-url https://test.pypi.org/simple/ gatews"
        ;;
    2)
        echo "上传到 PyPI..."
        twine upload dist/*
        echo ""
        echo "✅ 发布成功!"
        echo "安装命令: pip install --upgrade gatews"
        ;;
    3)
        echo "上传到 TestPyPI..."
        twine upload --repository testpypi dist/*
        echo ""
        echo "上传到 PyPI..."
        twine upload dist/*
        echo ""
        echo "✅ 全部发布成功!"
        echo "安装命令: pip install --upgrade gatews"
        ;;
    *)
        echo "无效的选项"
        exit 1
        ;;
esac

echo ""
echo "发布完成! 版本: $VERSION"
echo "PyPI 页面: https://pypi.org/project/gatews/"