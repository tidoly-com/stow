#!/bin/sh
# Builds the coupon helper next to this script: ./stow-coupons
cd "$(dirname "$0")" && swiftc -O main.swift -o stow-coupons
