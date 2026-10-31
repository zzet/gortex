//go:build grammardump

package forest

import (
	"fmt"
	"sort"
	"testing"
	"unsafe"

	adaforest "github.com/alexaandru/go-sitter-forest/ada"
	apexforest "github.com/alexaandru/go-sitter-forest/apex"
	bladeforest "github.com/alexaandru/go-sitter-forest/blade"
	clojureforest "github.com/alexaandru/go-sitter-forest/clojure"
	cmakeforest "github.com/alexaandru/go-sitter-forest/cmake"
	cobolforest "github.com/alexaandru/go-sitter-forest/cobol"
	crystalforest "github.com/alexaandru/go-sitter-forest/crystal"
	dforest "github.com/alexaandru/go-sitter-forest/d"
	elispforest "github.com/alexaandru/go-sitter-forest/elisp"
	erlangforest "github.com/alexaandru/go-sitter-forest/erlang"
	fortranforest "github.com/alexaandru/go-sitter-forest/fortran"
	fsharpforest "github.com/alexaandru/go-sitter-forest/fsharp"
	gdscriptforest "github.com/alexaandru/go-sitter-forest/gdscript"
	groovyforest "github.com/alexaandru/go-sitter-forest/groovy"
	hareforest "github.com/alexaandru/go-sitter-forest/hare"
	haskellforest "github.com/alexaandru/go-sitter-forest/haskell"
	jinjaforest "github.com/alexaandru/go-sitter-forest/jinja"
	juliaforest "github.com/alexaandru/go-sitter-forest/julia"
	liquidforest "github.com/alexaandru/go-sitter-forest/liquid"
	matlabforest "github.com/alexaandru/go-sitter-forest/matlab"
	moveforest "github.com/alexaandru/go-sitter-forest/move"
	nimforest "github.com/alexaandru/go-sitter-forest/nim"
	nixforest "github.com/alexaandru/go-sitter-forest/nix"
	alforest "github.com/alexaandru/go-sitter-forest/al"
	objcforest "github.com/alexaandru/go-sitter-forest/objc"
	odinforest "github.com/alexaandru/go-sitter-forest/odin"
	pascalforest "github.com/alexaandru/go-sitter-forest/pascal"
	perlforest "github.com/alexaandru/go-sitter-forest/perl"
	powershellforest "github.com/alexaandru/go-sitter-forest/powershell"
	pugforest "github.com/alexaandru/go-sitter-forest/pug"
	racketforest "github.com/alexaandru/go-sitter-forest/racket"
	rescriptforest "github.com/alexaandru/go-sitter-forest/rescript"
	solidityforest "github.com/alexaandru/go-sitter-forest/solidity"
	tactforest "github.com/alexaandru/go-sitter-forest/tact"
	tclforest "github.com/alexaandru/go-sitter-forest/tcl"
	twigforest "github.com/alexaandru/go-sitter-forest/twig"
	valaforest "github.com/alexaandru/go-sitter-forest/vala"
	vimforest "github.com/alexaandru/go-sitter-forest/vim"
	zigforest "github.com/alexaandru/go-sitter-forest/zig"

	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// TestDumpGrammarKinds is a research helper. It parses a small
// fixture per grammar and prints every named node kind seen, with
// frequencies — feeds the per-language node-kind dispatch table.
// Run with: go test -tags grammardump -run TestDumpGrammarKinds -v ./internal/parser/forest/
func TestDumpGrammarKinds(t *testing.T) {
	cases := []struct {
		name string
		get  func() unsafe.Pointer
		src  string
	}{
		{"erlang", erlangforest.GetLanguage, `-module(m).
-export([add/2]).
add(A, B) -> A + B.
multiply(A, B) -> A * B.
`},
		{"haskell", haskellforest.GetLanguage, `module M where
import Data.List
data Color = Red | Green
type Name = String
type Point = (Int, Int)
newtype Wrap a = Wrap a
class Show a where
  show :: a -> String
instance Show Color where
  show Red = "red"
add :: Int -> Int -> Int
add x y = x + y
`},
		{"crystal", crystalforest.GetLanguage, `module Greeter
  class Hello
    def say
      "hi"
    end
  end
  struct Point
  end
end
`},
		{"nim", nimforest.GetLanguage, `import strutils
import sequtils, tables

type
  Point* = object
    x*, y*: int

  Shape = enum
    Circle, Square

proc distance*(a, b: Point): float =
  return 0.0

func double(n: int): int =
  n * 2
`},
		{"ada", adaforest.GetLanguage, `with Ada.Text_IO; use Ada.Text_IO;
package body M is
   procedure Hello is begin Put_Line ("hi"); end Hello;
   function Add (X, Y : Integer) return Integer is begin return X + Y; end Add;
end M;
`},
		{"fortran", fortranforest.GetLanguage, `module greetings
contains
  subroutine hello
    print *, 'hi'
  end subroutine hello
  function add(a, b)
    integer :: a, b, add
    add = a + b
  end function add
end module greetings
`},
		{"vim", vimforest.GetLanguage, `function! Hello() abort
  echo 'hi'
endfunction
function! s:add(a, b)
  return a:a + a:b
endfunction
`},
		{"tcl", tclforest.GetLanguage, `proc hello {} { puts hi }
proc add {a b} { return [expr {$a + $b}] }
package require Tcl 8.6
`},
		{"perl", perlforest.GetLanguage, `package M;
use strict;
sub hello { print "hi\n" }
sub add { my ($a,$b) = @_; return $a+$b; }
1;
`},
		{"powershell", powershellforest.GetLanguage, `function Hello { Write-Host "hi" }
function Add($a, $b) { return $a + $b }
class Greeter { [string]$Name }
`},
		{"pascal", pascalforest.GetLanguage, `program M;
type Point = record x, y: integer; end;
procedure Hello; begin writeln('hi'); end;
function Add(a, b: integer): integer; begin Add := a + b; end;
begin Hello; end.
`},
		{"odin", odinforest.GetLanguage, `package m
import "core:fmt"
Point :: struct { x, y: int }
hello :: proc() { fmt.println("hi") }
add :: proc(a, b: int) -> int { return a + b }
`},
		{"hare", hareforest.GetLanguage, `use io;
type point = struct { x: int, y: int };
fn add(a: int, b: int) int = a + b;
export fn hello() void = io::println("hi");
`},
		{"zig", zigforest.GetLanguage, `const std = @import("std");
const Point = struct { x: i32, y: i32 };
fn add(a: i32, b: i32) i32 { return a + b; }
pub fn hello() void {}
`},
		{"d", dforest.GetLanguage, `module m;
import std.stdio;
struct Point { int x, y; }
class Greeter { void hello() {} }
int add(int a, int b) { return a + b; }
`},
		{"vala", valaforest.GetLanguage, `using GLib;
namespace M { class Greeter : Object { public void hello() {} } }
`},
		{"groovy", groovyforest.GetLanguage, `package m
class Greeter { String hello() { return 'hi' } }
def add(a, b) { return a + b }
`},
		{"clojure", clojureforest.GetLanguage, `(ns m (:require [clojure.string :as s]))
(defn hello [] (println "hi"))
(defn add [a b] (+ a b))
(defrecord Point [x y])
`},
		{"cmake", cmakeforest.GetLanguage, `cmake_minimum_required(VERSION 3.10)
function(hello)
  message("hi")
endfunction()
macro(double x)
  set(${x}_2 ${${x}}*2)
endmacro()
`},
		{"cobol", cobolforest.GetLanguage, `IDENTIFICATION DIVISION.
       PROGRAM-ID. M.
       PROCEDURE DIVISION.
           DISPLAY 'hi'.
           STOP RUN.
`},
		{"julia", juliaforest.GetLanguage, `module M
using LinearAlgebra
struct Point x::Int; y::Int end
function hello() println("hi") end
add(a, b) = a + b
end
`},
		{"matlab", matlabforest.GetLanguage, `function out = add(a, b)
  out = a + b;
end
classdef Greeter
  methods
    function hello(obj); disp('hi'); end
  end
end
`},
		{"apex", apexforest.GetLanguage, `public class Greeter {
  public String hello() { return 'hi'; }
  public Integer add(Integer a, Integer b) { return a + b; }
}
trigger AccountTrigger on Account (before insert) {}
`},
		{"solidity", solidityforest.GetLanguage, `pragma solidity ^0.8.0;
import "./X.sol";
interface IFoo { function bar() external; }
contract Token { function mint(address to, uint256 amt) public {} event Tx(); modifier ok() { _; } struct Holder {} enum State {} }
`},
		{"tact", tactforest.GetLanguage, `import "./other";
trait T { fun base() {} }
contract Greeter with T {
  init() { }
  fun hello(): String { return "hi"; }
  receive("ping") {}
}
`},
		{"move", moveforest.GetLanguage, `module 0x1::M {
  use std::vector;
  struct Counter has key { value: u64 }
  public fun increment(c: &mut Counter) { c.value = c.value + 1; }
}
`},
		{"racket", racketforest.GetLanguage, `#lang racket
(require racket/string)
(define (hello) (displayln "hi"))
(define (add a b) (+ a b))
(struct point (x y))
`},
		{"elisp", elispforest.GetLanguage, `(require 'cl-lib)
(defun hello () "hi")
(defun add (a b) (+ a b))
(defvar greeting "hi")
(defmacro when-bind (b &rest body) (list 'let b body))
`},
		{"fsharp", fsharpforest.GetLanguage, `module M
open System
type Point = { x: int; y: int }
let hello () = printfn "hi"
let add a b = a + b
`},
		{"gdscript", gdscriptforest.GetLanguage, `extends Node
class_name Greeter
signal greeted(name)
func hello():
    print("hi")
func add(a: int, b: int) -> int:
    return a + b
`},
		{"jinja", jinjaforest.GetLanguage, `{% extends 'base.html' %}
{% block content %}
  {% macro greet(name) %}Hi, {{ name }}{% endmacro %}
  {{ greet('world') }}
{% endblock %}
`},
		{"liquid", liquidforest.GetLanguage, `{% include 'header' %}
{% capture greeting %}Hi{% endcapture %}
{% assign name = "world" %}
{{ greeting }}, {{ name }}
`},
		{"twig", twigforest.GetLanguage, `{% extends 'base.twig' %}
{% block content %}
  {% macro greet(name) %}Hi, {{ name }}{% endmacro %}
{% endblock %}
`},
		{"pug", pugforest.GetLanguage, `extends layout
block content
  mixin greet(name)
    p Hi #{name}
  +greet('world')
`},
		{"blade", bladeforest.GetLanguage, `@extends('layouts.app')
@section('content')
  @include('partials.header')
  <p>Hi {{ $name }}</p>
@endsection
`},
		{"rescript", rescriptforest.GetLanguage, `module M = {
  type point = { x: int, y: int }
  let hello = () => Js.log("hi")
  let add = (a, b) => a + b
}
`},
		{"nix", nixforest.GetLanguage, `{ lib, ... }:
let
  greet = name: "Hi, ${name}";
  add = a: b: a + b;
in {
  inherit greet add;
}
`},
		{"objc", objcforest.GetLanguage, `#import <Foundation/Foundation.h>
@interface Greeter : NSObject
- (NSString *)hello;
@end
@implementation Greeter
- (NSString *)hello { return @"hi"; }
@end
`},
		{"al", alforest.GetLanguage, `table 50000 Customer
{
    fields { field(1; Name; Text[100]) {} }
}
codeunit 50001 CustomerMgt
{
    procedure Hello() begin Message('hi'); end;
}
page 50002 CustList { SourceTable = Customer; }
`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lang := sitter.NewLanguage(c.get())
			tree, err := parser.ParseFile([]byte(c.src), lang)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			defer tree.Close()

			kinds := make(map[string]int)
			walkKinds(tree.RootNode(), kinds)

			keys := make([]string, 0, len(kinds))
			for k := range kinds {
				keys = append(keys, k)
			}
			sort.Strings(keys)

			fmt.Printf("=== %s ===\n", c.name)
			for _, k := range keys {
				fmt.Printf("  %-40s × %d\n", k, kinds[k])
			}
		})
	}
}

func walkKinds(n *sitter.Node, kinds map[string]int) {
	if n == nil {
		return
	}
	if n.IsNamed() {
		kinds[n.Type()]++
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		walkKinds(n.NamedChild(i), kinds)
	}
}
